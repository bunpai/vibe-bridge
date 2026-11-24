package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolatedState(t *testing.T) {
	t.Helper()
	t.Setenv("MISTRAL_BRIDGE_DATA_DIR", t.TempDir())
	state = pluginState{Keys: []apiKey{}, Models: []modelMapping{}, Catalog: []string{}, Logs: []RequestLog{}}
	stateLoaded = true
	publishedRevision = ""
	original := hostCall
	t.Cleanup(func() { hostCall = original })
}

func managementCall(t *testing.T, method, path, body string) managementResponse {
	t.Helper()
	raw, _ := json.Marshal(managementRequest{Method: method, Path: "/plugins/vibe-bridge" + path, Body: []byte(body)})
	reply, err := handleMethod("management.handle", raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(reply, &env); err != nil || !env.OK {
		t.Fatalf("response = %s, %v", reply, err)
	}
	var result managementResponse
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFreshInstallReportsEmptySynchronizedState(t *testing.T) {
	isolatedState(t)
	stateLoaded = false
	if _, err := staticModels(nil); err != nil {
		t.Fatal(err)
	}
	response := managementCall(t, "GET", "/state", "")
	if response.StatusCode != http.StatusOK {
		t.Fatal(string(response.Body))
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(response.Body, &snapshot); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"keys", "models", "catalog"} {
		if string(snapshot[field]) != "[]" {
			t.Fatalf("fresh %s must be an empty array, got %s", field, snapshot[field])
		}
	}
	if string(snapshot["models_revision"]) != string(snapshot["published_revision"]) {
		t.Fatal("fresh installation incorrectly reports pending model synchronization")
	}
	path, err := statePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("opening the console should leave initial setup to the user")
	}
}

func TestMappingRegistrationRenameAndCatalogSync(t *testing.T) {
	isolatedState(t)
	state.Keys = []apiKey{{ID: "key", Secret: "test", Enabled: true}}
	response := managementCall(t, "POST", "/models", `{"client_model":"short","upstream_model":"long-upstream-name","enabled":true}`)
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	raw, _ := staticModels(nil)
	if !strings.Contains(string(raw), `"ID":"short"`) || strings.Contains(string(raw), `"ID":"vibe-bridge/short"`) {
		t.Fatal(string(raw))
	}
	for _, model := range []string{"short", "vibe-bridge/short"} {
		route, _ := routeModel([]byte(`{"RequestedModel":"` + model + `"}`))
		if !strings.Contains(string(route), `"Handled":true`) {
			t.Fatal(string(route))
		}
	}
	id := currentState().Models[0].ID
	response = managementCall(t, "POST", "/models", `{"id":"`+id+`","client_model":"renamed","upstream_model":"long-upstream-name","enabled":true}`)
	if response.StatusCode != 200 {
		t.Fatal(string(response.Body))
	}
	if publicState()["models_revision"] == publicState()["published_revision"] {
		t.Fatal("unpublished edit was reported as registered")
	}
	raw, _ = staticModels(nil)
	if strings.Contains(string(raw), `"ID":"short"`) || !strings.Contains(string(raw), `"ID":"renamed"`) {
		t.Fatal(string(raw))
	}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		response, _ := json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(`{"data":[{"id":"new-upstream","capabilities":{"completion_chat":true}}]}`)})
		return response, nil
	}
	response = managementCall(t, "POST", "/models/sync", `{"key_id":"key"}`)
	if response.StatusCode != 200 || len(currentState().Models) != 1 || currentState().Models[0].ClientModel != "renamed" {
		t.Fatal("catalog sync changed mappings")
	}
	response = managementCall(t, "DELETE", "/models/delete", `{"id":"`+id+`"}`)
	raw, _ = staticModels(nil)
	if response.StatusCode != 200 || strings.Contains(string(raw), "renamed") {
		t.Fatal("deleted mapping remains registered")
	}
}

func TestPrioritySelectionAndExplicitBinding(t *testing.T) {
	settings := pluginState{Models: []modelMapping{{ClientModel: "alias", Enabled: true}}, Keys: []apiKey{{ID: "low", Priority: 0, Enabled: true}, {ID: "high1", Priority: 5, Enabled: true}, {ID: "disabled", Priority: 10}, {ID: "high2", Priority: 5, Enabled: true}}}
	used := map[string]bool{}
	for i := 0; i < 8; i++ {
		key, _, ok := resolveKeyAndModel(settings, "alias")
		if !ok || key.Priority != 5 {
			t.Fatalf("key = %+v", key)
		}
		used[key.ID] = true
	}
	if len(used) != 2 {
		t.Fatal("same-priority keys did not rotate")
	}
	settings.Models[0].KeyID = "low"
	key, _, ok := resolveKeyAndModel(settings, "alias")
	if !ok || key.ID != "low" {
		t.Fatal("explicit binding ignored")
	}
	settings.Models[0].KeyID = "disabled"
	if _, _, ok := resolveKeyAndModel(settings, "alias"); ok {
		t.Fatal("disabled bound key selected")
	}
}

func TestCredentialsPersistenceDuplicatesAndFailureRollback(t *testing.T) {
	isolatedState(t)
	response := managementCall(t, "POST", "/keys", `{"label":"primary","secret":"test-secret","priority":5}`)
	if response.StatusCode != 200 || strings.Contains(string(response.Body), "test-secret") {
		t.Fatal(string(response.Body))
	}
	response = managementCall(t, "POST", "/keys", `{"secret":"test-secret"}`)
	if response.StatusCode != 409 || len(currentState().Keys) != 1 {
		t.Fatal("duplicate key accepted")
	}
	key := currentState().Keys[0]
	response = managementCall(t, "PATCH", "/keys", `{"id":"`+key.ID+`","label":"updated","priority":10,"enabled":true}`)
	if response.StatusCode != 200 || currentState().Keys[0].Secret != "test-secret" {
		t.Fatal("editing erased secret")
	}
	stateLoaded = false
	loadState()
	if currentState().Keys[0].Priority != 10 {
		t.Fatal("priority not persisted")
	}
	path, _ := statePath()
	if err := os.Mkdir(path+".tmp", 0700); err != nil {
		t.Fatal(err)
	}
	response = managementCall(t, "POST", "/keys", `{"secret":"another"}`)
	if response.StatusCode != 500 || len(currentState().Keys) != 1 {
		t.Fatal("failed write changed state")
	}
}

func TestConcurrentKeySaves(t *testing.T) {
	isolatedState(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = saveKey([]byte(`{"secret":"same"}`), false) }()
	}
	wg.Wait()
	if len(currentState().Keys) != 1 {
		t.Fatal("concurrent saves duplicated a key")
	}
}

func TestLogFiltersStatsAndLegacyRecords(t *testing.T) {
	first := int64(200)
	cache := 40
	now := time.Now().UTC()
	logs := []RequestLog{
		{ID: "old", CreatedAt: now.Add(-48 * time.Hour), Model: "alias", InputTokens: 30, OutputTokens: 2},
		{ID: "one", CreatedAt: now.Add(-time.Hour), Model: "alias", UpstreamModel: "upstream", KeyID: "key", KeyLabel: "primary", Thinking: "high", Status: "ok", Stream: true, FirstTokenMS: &first, DurationMS: 1200, InputTokens: 100, OutputTokens: 20, CachedTokens: &cache},
		{ID: "two", CreatedAt: now, Model: "alias", Status: "error", HTTPStatus: 429, Error: "limited"},
	}
	filter := logFilter{From: now.Add(-24 * time.Hour), Page: 1, PageSize: 1}
	result := filterRequestLogs(logs, filter)
	stats := result["stats"].(logStats)
	if stats.Requests != 2 || stats.Total != 120 || stats.CacheRate == nil || *stats.CacheRate != 40 || stats.TokenRate == nil || *stats.TokenRate != 20 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(result["logs"].([]RequestLog)) != 1 || result["pages"] != 2 {
		t.Fatal("pagination failed")
	}
	filter.Search = "upstream"
	filter.KeyID = "key"
	filter.Thinking = "high"
	filter.Status = "ok"
	if filterRequestLogs(logs, filter)["total"] != 1 {
		t.Fatal("combined filters failed")
	}
	filter.To = logs[1].CreatedAt
	if filterRequestLogs(logs, filter)["total"] != 0 {
		t.Fatal("range end must be exclusive")
	}
	legacy := filterRequestLogs(logs[:1], logFilter{Page: 1, PageSize: 50})["stats"].(logStats)
	if legacy.Total != 32 || legacy.CacheRate != nil || legacy.TokenRate != nil {
		t.Fatal("invented missing legacy metrics")
	}
}

func TestUsageDetailsAndFirstToken(t *testing.T) {
	usage := parseUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":0},"num_cached_tokens":40}}`))
	if usage["prompt_tokens"] != 100 || usage["cached_tokens"] != 40 {
		t.Fatalf("usage = %#v", usage)
	}
	trace := newRequestTrace(rpcExecutorRequest{Stream: true})
	trace.observe([]byte(`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`))
	if trace.entry.FirstTokenMS != nil {
		t.Fatal("role-only event counted as first token")
	}
	trace.observe([]byte(`data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}`))
	if trace.entry.FirstTokenMS == nil {
		t.Fatal("reasoning token ignored")
	}
}

func TestQuotaUsesCorrectBudgetAndProxyCallback(t *testing.T) {
	isolatedState(t)
	kind := "api"
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(upstreamRequest)
		if method != "host.http.do" || req.HostCallbackID != "proxy-context" || req.Headers.Get("Authorization") != "Bearer test" {
			t.Fatal("quota bypassed CPA HTTP bridge")
		}
		body := `{"plan_type":"` + kind + `","plan_name":"INDIVIDUAL"}`
		if strings.HasSuffix(req.URL, "budget") {
			body = `{"vibe_budget":{"initial_budget":300,"usage_percentage":10},"api_budget":{"initial_budget":50,"usage_percentage":40,"currency":"EUR","payg_enabled":true}}`
		}
		if strings.HasSuffix(req.URL, "subscription") {
			body = `{"plan_name":"INDIVIDUAL","payg_enabled":false}`
		}
		return json.Marshal(upstreamResponse{StatusCode: http.StatusOK, Body: []byte(body)})
	}
	quota, err := fetchQuota(apiKey{Secret: "test"}, "proxy-context")
	if err != nil || quota.Limit != 50 || quota.Remaining != 30 || !quota.PayAsYouGo {
		t.Fatalf("quota = %+v, %v", quota, err)
	}
	kind = "chat"
	quota, err = fetchQuota(apiKey{Secret: "test"}, "proxy-context")
	if err != nil || quota.Limit != 300 || quota.Remaining != 270 {
		t.Fatalf("quota = %+v, %v", quota, err)
	}
}

func TestCompletionLogsResolvedFieldsAndErrors(t *testing.T) {
	isolatedState(t)
	state.Keys = []apiKey{{ID: "key", Secret: "secret", Label: "primary", Enabled: true}}
	state.Models = []modelMapping{{ClientModel: "alias", UpstreamModel: "upstream", ThinkingMap: "high=medium", Enabled: true}}
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(upstreamRequest)
		if !strings.Contains(string(req.Body), `"model":"upstream"`) {
			t.Fatal("alias not resolved")
		}
		return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)})
	}
	req, _ := json.Marshal(rpcExecutorRequest{Model: "alias", Payload: []byte(`{"messages":[],"reasoning_effort":"high"}`)})
	if _, err := execute(req); err != nil {
		t.Fatal(err)
	}
	row := state.Logs[0]
	if row.Model != "alias" || row.UpstreamModel != "upstream" || row.KeyID != "key" || row.Thinking != "medium" || row.TotalTokens != 7 {
		t.Fatalf("log = %+v", row)
	}
	hostCall = func(string, any) (json.RawMessage, error) { return nil, errors.New("network failure") }
	if _, err := execute(req); err == nil {
		t.Fatal("expected failure")
	}
	if len(state.Logs) != 2 || state.Logs[1].Status != "error" || state.Logs[1].KeyLabel != "primary" {
		t.Fatal("missing error attribution")
	}
	stored, err := os.ReadFile(filepath.Join(os.Getenv("MISTRAL_BRIDGE_DATA_DIR"), "requests.jsonl"))
	if err != nil || strings.Contains(string(stored), `"secret"`) {
		t.Fatal("log persistence or redaction failed")
	}
}
