package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func seedSearchState() {
	state.Keys = []apiKey{{ID: "searchkey", Secret: "secret", Label: "primary", Enabled: true}}
	state.Models = []modelMapping{{ClientModel: "glmflash", UpstreamModel: "zai-glm-5-3-flash", Enabled: true}}
}

func TestSearchModelResolvesAndRoutes(t *testing.T) {
	isolatedState(t)
	seedSearchState()

	key, model, ok := resolveKeyAndModel(currentState(), "vibe-search")
	if !ok {
		t.Fatal("vibe-search did not resolve")
	}
	if key.ID != "searchkey" {
		t.Fatalf("key = %s", key.ID)
	}
	if model.ClientModel != "vibe-search" || model.UpstreamModel != "zai-glm-5-3-flash" {
		t.Fatalf("model = %+v", model)
	}
	// plugin-prefixed name resolves too
	if _, _, ok := resolveKeyAndModel(currentState(), pluginID+"/vibe-search"); !ok {
		t.Fatal("prefixed vibe-search did not resolve")
	}

	// registration: staticModels must list it
	raw, err := staticModels(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "vibe-search") {
		t.Fatalf("staticModels missing vibe-search: %s", raw)
	}

	// routing: handled
	req, _ := json.Marshal(map[string]string{"RequestedModel": "vibe-search"})
	reply, err := routeModel(req)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(reply, &env)
	var routed struct {
		Handled bool
	}
	_ = json.Unmarshal(env.Result, &routed)
	if !routed.Handled {
		t.Fatalf("routeModel did not handle vibe-search: %s", reply)
	}

	// connector_mode=off hides it entirely
	state.ConnectorMode = "off"
	raw, _ = staticModels(nil)
	if strings.Contains(string(raw), "vibe-search") {
		t.Fatal("staticModels still lists vibe-search in off mode")
	}
	reply, _ = routeModel(req)
	_ = json.Unmarshal(reply, &env)
	routed.Handled = true
	_ = json.Unmarshal(env.Result, &routed)
	if routed.Handled {
		t.Fatal("routeModel still handles vibe-search in off mode")
	}
}

func TestSearchModelActivatesLoopInDirectiveMode(t *testing.T) {
	isolatedState(t)
	seedSearchState()
	// connector_mode defaults to "directive" — the search model must still loop.

	round := 0
	var firstBody string
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		req := payload.(upstreamRequest)
		switch {
		case strings.Contains(req.URL, "connectors/bootstrap"):
			return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(
				`{"connectors":[{"id":"cid-1","name":"web_search","status":{"is_ready":true}}]}`)})
		case strings.Contains(req.URL, "connectors-gateway"):
			return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(
				`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"SEARCH_HITS"}]}}`)})
		case strings.Contains(req.URL, "chat/completions"):
			round++
			if round == 1 {
				firstBody = string(req.Body)
				return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(
					`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"connector_web_search.web_search","arguments":"{\"query\":\"test\"}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)})
			}
			return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(
				`{"choices":[{"message":{"role":"assistant","content":"final answer"}}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`)})
		}
		return json.Marshal(upstreamResponse{StatusCode: 404, Body: []byte(`{"error":"unknown url"}`)})
	}

	// plain request to vibe-search — no x_vibe_connectors marker needed
	req, _ := json.Marshal(rpcExecutorRequest{Model: "vibe-search", Payload: []byte(`{"messages":[{"role":"user","content":"what is new today"}]}`)})
	raw, err := execute(req)
	if err != nil {
		t.Fatal(err)
	}

	// connector tools must have been injected into the first upstream request
	if !strings.Contains(firstBody, "connector_web_search.web_search") {
		t.Fatalf("connector tools not injected, body = %s", firstBody)
	}
	// the loop must have run: round 2 happened and final content surfaced
	if round < 2 {
		t.Fatalf("loop did not continue, rounds = %d", round)
	}
	var env struct {
		Result struct {
			Payload []byte `json:"Payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env.Result.Payload), "final answer") {
		t.Fatalf("final result missing answer: %s", raw)
	}
	// the replayed body must include the tool result
	if !strings.Contains(firstBody, "zai-glm-5-3-flash") {
		t.Fatal("backing upstream model not used")
	}
}

func TestSearchModelRequiresBacking(t *testing.T) {
	isolatedState(t)
	state.Keys = []apiKey{{ID: "searchkey", Secret: "secret", Enabled: true}}
	state.Models = []modelMapping{} // no backing mapping
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		return json.Marshal(upstreamResponse{StatusCode: 200, Body: []byte(`{"choices":[{"message":{"content":"ok"}}]}`)})
	}
	req, _ := json.Marshal(rpcExecutorRequest{Model: "vibe-search", Payload: []byte(`{"messages":[]}`)})
	_, err := execute(req)
	if err == nil || !strings.Contains(err.Error(), "backing") {
		t.Fatalf("expected backing error, got %v", err)
	}
}

func TestCompletionMessageTextExtraction(t *testing.T) {
	plain := []byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":1}}`)
	got := parseCompletionParts(plain)
	if got.Content != "hello" || got.Reasoning != "" || len(got.Usage) == 0 {
		t.Fatalf("plain = %+v", got)
	}
	withReasoning := []byte(`{"choices":[{"message":{"reasoning_content":"think","content":"answer"}}]}`)
	got = parseCompletionParts(withReasoning)
	if got.Reasoning != "think" || got.Content != "answer" {
		t.Fatalf("reasoning = %+v", got)
	}
	parts := []byte(`{"choices":[{"message":{"content":[{"type":"thinking","text":"t"},{"type":"text","text":"x"}]}}]}`)
	got = parseCompletionParts(parts)
	if got.Reasoning != "t" || got.Content != "x" {
		t.Fatalf("parts = %+v", got)
	}
	if got := parseCompletionParts([]byte(`{}`)); got.Content != "" || got.Reasoning != "" {
		t.Fatalf("empty = %+v", got)
	}
}
