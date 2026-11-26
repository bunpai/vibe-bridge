package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pluginID   = "vibe-bridge"
	pluginName = "Mistral Bridge"
)

// Release builds set this to the tag version with -ldflags -X.
var pluginVersion = "0.1.3"

//go:embed web/index.html web/style.css web/app.js
var webFiles embed.FS

//go:embed logo.png
var pluginLogo []byte

var pluginLogoURI = "data:image/png;base64," + base64.StdEncoding.EncodeToString(pluginLogo)

var consolePage = func() []byte {
	page, _ := webFiles.ReadFile("web/index.html")
	css, _ := webFiles.ReadFile("web/style.css")
	script, _ := webFiles.ReadFile("web/app.js")
	html := strings.Replace(string(page), "/* BRIDGE_STYLES */", string(css), 1)
	html = strings.ReplaceAll(html, "BRIDGE_LOGO_URI", pluginLogoURI)
	return []byte(strings.Replace(html, "/* BRIDGE_SCRIPT */", string(script), 1))
}()

type apiKey struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Secret    string     `json:"secret"`
	BaseURL   string     `json:"base_url,omitempty"`
	Enabled   bool       `json:"enabled"`
	Priority  int        `json:"priority"`
	Channel   string     `json:"channel,omitempty"`
	Source    string     `json:"source,omitempty"`
	Quotas    *keyQuotas `json:"quotas,omitempty"`
	Quota     *keyQuota  `json:"quota,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type modelMapping struct {
	ID            string `json:"id"`
	ClientModel   string `json:"client_model"`
	UpstreamModel string `json:"upstream_model"`
	ThinkingMap   string `json:"thinking_map,omitempty"`
	ImageLimit    int    `json:"image_limit,omitempty"`
	KeyID         string `json:"key_id,omitempty"`
	Enabled       bool   `json:"enabled"`
}

type RequestLog struct {
	ID             string    `json:"id"`
	Model          string    `json:"model"`
	UpstreamModel  string    `json:"upstream_model,omitempty"`
	Thinking       string    `json:"thinking,omitempty"`
	KeyID          string    `json:"key_id,omitempty"`
	KeyLabel       string    `json:"key_label,omitempty"`
	KeyMasked      string    `json:"key_masked,omitempty"`
	Stream         bool      `json:"stream,omitempty"`
	FirstTokenMS   *int64    `json:"first_token_ms,omitempty"`
	CachedTokens   *int      `json:"cached_tokens,omitempty"`
	TotalTokens    int       `json:"total_tokens,omitempty"`
	UsageKnown     bool      `json:"usage_known,omitempty"`
	Error          string    `json:"error,omitempty"`
	Status         string    `json:"status"`
	HTTPStatus     int       `json:"http_status,omitempty"`
	DebugAvailable bool      `json:"debug_available,omitempty"`
	DurationMS     int64     `json:"duration_ms"`
	InputTokens    int       `json:"input_tokens,omitempty"`
	OutputTokens   int       `json:"output_tokens,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type pluginState struct {
	Keys       []apiKey       `json:"keys"`
	Catalog    []string       `json:"catalog"`
	Models     []modelMapping `json:"models"`
	DebugMode  bool           `json:"debug_mode,omitempty"`
	ProxyURL   string         `json:"proxy_url,omitempty"`
	ImageLimit int            `json:"image_limit,omitempty"`
	ConnectorMode string      `json:"connector_mode,omitempty"`
	Logs       []RequestLog   `json:"-"`
}

type safeKey struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	Masked        string     `json:"masked"`
	BaseURL       string     `json:"base_url"`
	Enabled       bool       `json:"enabled"`
	Priority      int        `json:"priority"`
	Channel       string     `json:"channel"`
	Source        string     `json:"source,omitempty"`
	ActiveChannel string     `json:"active_channel,omitempty"`
	Quotas        *keyQuotas `json:"quotas,omitempty"`
	Quota         *keyQuota  `json:"quota,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type managementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

var (
	stateMu           sync.RWMutex
	state             = pluginState{Keys: []apiKey{}, Catalog: []string{}, Models: []modelMapping{}, Logs: []RequestLog{}}
	dataMu            sync.Mutex
	logMu             sync.Mutex
	stateLoaded       bool
	dataDir           string
	keyTurn           atomic.Uint64
	publishedRevision string
	hostCall          = callHost
)

func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return registration()
	case "executor.identifier":
		return okEnvelope(map[string]string{"identifier": pluginID})
	case "model.static":
		return staticModels(raw)
	case "model.for_auth":
		return staticModels(nil)
	case "model.route":
		return routeModel(raw)
	case "executor.execute":
		return execute(raw)
	case "executor.execute_stream":
		return executeStream(raw)
	case "executor.count_tokens":
		return nil, &statusError{Status: http.StatusNotImplemented, Message: "Token counting is not supported by this Mistral API route"}
	case "management.register":
		return okEnvelope(map[string]any{"resources": []map[string]string{{"Path": "/console", "Menu": pluginName, "Description": "Manage Mistral keys, models and request logs."}}, "routes": managementRoutes()})
	case "management.handle":
		return handleManagement(raw)
	case "thinking.identifier":
		return okEnvelope(map[string]string{"identifier": pluginID})
	case "thinking.apply":
		return applyThinking(raw)
	case "usage.handle":
		return okEnvelope(map[string]any{})
	default:
		return nil, fmt.Errorf("unknown plugin method %s", method)
	}
}

func registration() ([]byte, error) {
	return okEnvelope(map[string]any{
		"schema_version": 10,
		"metadata": map[string]any{
			"Name":             pluginName,
			"Version":          pluginVersion,
			"Author":           "RzMY",
			"GitHubRepository": "https://github.com/RzMY/MistralBridge",
			"Logo":             pluginLogoURI,
			"ConfigFields":     []any{},
		},
		"capabilities": map[string]any{
			"model_provider":          true,
			"model_router":            true,
			"executor":                true,
			"executor_model_scope":    "static",
			"executor_input_formats":  []string{"chat-completions"},
			"executor_output_formats": []string{"chat-completions"},
			"management_api":          true,
			"thinking_applier":        true,
			"usage_plugin":            true,
		},
	})
}

func staticModels(raw []byte) ([]byte, error) {
	var request struct {
		Host struct {
			AuthDir string
		}
	}
	if len(raw) > 0 && json.Unmarshal(raw, &request) == nil {
		configureDataDirectory(request.Host.AuthDir)
	}
	loadState()
	stateMu.Lock()
	models := make([]map[string]any, 0, len(state.Models))
	for _, model := range state.Models {
		if !model.Enabled {
			continue
		}
		models = append(models, modelInfo(model))
	}
	publishedRevision = modelRevision(state.Models)
	stateMu.Unlock()
	return okEnvelope(map[string]any{"Provider": pluginID, "Models": models})
}

func modelInfo(model modelMapping) map[string]any {
	id := strings.TrimPrefix(model.ClientModel, pluginID+"/")
	return map[string]any{
		"ID":                         id,
		"Object":                     "model",
		"OwnedBy":                    pluginID,
		"DisplayName":                model.ClientModel,
		"Name":                       id,
		"SupportedGenerationMethods": []string{"chat"},
		"ContextLength":              128000,
		"MaxCompletionTokens":        8192,
		"SupportedParameters":        []string{"temperature", "top_p", "max_tokens", "reasoning_effort", "tools", "tool_choice", "stream"},
	}
}

func routeModel(raw []byte) ([]byte, error) {
	var req struct {
		RequestedModel string `json:"RequestedModel"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	modelID := strings.TrimPrefix(req.RequestedModel, pluginID+"/")
	if modelID == "" {
		return okEnvelope(map[string]any{"Handled": false})
	}
	state := currentState()
	for _, model := range state.Models {
		if model.Enabled && model.ClientModel == modelID {
			return okEnvelope(map[string]any{"Handled": true, "TargetKind": "self", "Reason": "mistral_bridge_model"})
		}
	}
	return okEnvelope(map[string]any{"Handled": false})
}

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	trace := newRequestTrace(req)
	result, headers, err := performCompletion(nil, req, trace)
	trace.finish(parseUsage(result), err)
	if err != nil {
		return nil, err
	}
	return okEnvelope(map[string]any{"Payload": result, "Headers": headers})
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	req.Stream = true
	trace := newRequestTrace(req)
	headers, err := performCompletionStream(nil, req, trace)
	if err != nil {
		trace.finish(nil, err)
		return nil, err
	}
	return okEnvelope(map[string]any{"Headers": headers})
}

func applyThinking(raw []byte) ([]byte, error) {
	var req struct {
		Body   []byte `json:"Body"`
		Config struct {
			Level string `json:"Level"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	body, err := setJSONField(req.Body, "reasoning_effort", mapThinking(req.Config.Level, ""))
	if err != nil {
		return nil, err
	}
	return okEnvelope(map[string]any{"Body": body})
}

func managementRoutes() []map[string]string {
	paths := []struct{ method, path string }{
		{"GET", "/plugins/vibe-bridge/state"},
		{"PATCH", "/plugins/vibe-bridge/proxy"},
		{"POST", "/plugins/vibe-bridge/keys"},
		{"PATCH", "/plugins/vibe-bridge/keys"},
		{"POST", "/plugins/vibe-bridge/keys/quota"},
		{"PATCH", "/plugins/vibe-bridge/keys/channel"},
		{"GET", "/plugins/vibe-bridge/logs"},
		{"GET", "/plugins/vibe-bridge/logs/debug"},
		{"PATCH", "/plugins/vibe-bridge/debug"},
		{"PATCH", "/plugins/vibe-bridge/connector-mode"},
		{"DELETE", "/plugins/vibe-bridge/debug"},
		{"PATCH", "/plugins/vibe-bridge/models"},
		{"PATCH", "/plugins/vibe-bridge/keys/toggle"},
		{"DELETE", "/plugins/vibe-bridge/keys/delete"},
		{"POST", "/plugins/vibe-bridge/keys/test"},
		{"POST", "/plugins/vibe-bridge/models/sync"},
		{"POST", "/plugins/vibe-bridge/models"},
		{"DELETE", "/plugins/vibe-bridge/models/delete"},
		{"DELETE", "/plugins/vibe-bridge/logs"},
	}
	routes := make([]map[string]string, 0, len(paths))
	for _, entry := range paths {
		routes = append(routes, map[string]string{"Method": entry.method, "Path": entry.path})
	}
	return routes
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	loadState()
	method := strings.ToUpper(req.Method)
	path := strings.TrimRight(req.Path, "/")
	if strings.HasSuffix(path, "/console") {
		if method != http.MethodGet {
			return managementResult(http.StatusMethodNotAllowed, []byte("method not allowed"))
		}
		return managementPage(consolePage)
	}
	path = strings.TrimPrefix(path, "/v0/management")
	path = strings.TrimPrefix(path, "/plugins/vibe-bridge")
	if path == "" {
		path = "/"
	}
	switch {
	case method == http.MethodGet && path == "/state":
		return managementJSON(publicState())
	case method == http.MethodPatch && path == "/proxy":
		return updateProxy(req.Body)
	case method == http.MethodPost && path == "/keys":
		return saveKey(req.Body, false)
	case method == http.MethodPatch && path == "/keys":
		return saveKey(req.Body, true)
	case method == http.MethodPatch && path == "/keys/channel":
		return switchKeyChannel(req.Body)
	case method == http.MethodPost && path == "/keys/quota":
		return refreshQuota(req.Body, req.HostCallbackID)
	case method == http.MethodGet && path == "/logs":
		return queryRequestLogs(req.Query)
	case method == http.MethodGet && path == "/logs/debug":
		return queryDebugLog(req.Query)
	case method == http.MethodPatch && path == "/debug":
		return updateDebugMode(req.Body)
	case method == http.MethodPatch && path == "/connector-mode":
		return updateConnectorMode(req.Body)
	case method == http.MethodDelete && path == "/debug":
		if err := clearDebugLogs(); err != nil {
			return managementJSONStatus(http.StatusInternalServerError, map[string]string{"error": "could not clear debug data"})
		}
		return managementJSON(map[string]bool{"ok": true})
	case method == http.MethodPatch && path == "/models":
		return updateMappings(req.Body)
	case method == http.MethodPost && path == "/keys/test":
		var input struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(req.Body, &input)
		return testKey(input.ID, req.HostCallbackID)
	case method == http.MethodPatch && path == "/keys/toggle":
		var input struct {
			ID      string `json:"id"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.Unmarshal(req.Body, &input); err != nil || strings.TrimSpace(input.ID) == "" {
			return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": "key id is required"})
		}
		return toggleKey(input.ID, input.Enabled)
	case method == http.MethodDelete && path == "/keys/delete":
		var input struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(req.Body, &input)
		return deleteKey(input.ID)
	case method == http.MethodPost && path == "/models/sync":
		return syncModels(req.Body, req.HostCallbackID)
	case method == http.MethodPost && path == "/models":
		return saveMapping(req.Body)
	case method == http.MethodDelete && path == "/models/delete":
		var input struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(req.Body, &input)
		return deleteMapping(input.ID)
	case method == http.MethodDelete && path == "/logs":
		if err := clearRequestLogs(); err != nil {
			return managementJSONStatus(http.StatusInternalServerError, map[string]string{"error": "could not save state"})
		}
		return managementJSON(map[string]bool{"ok": true})
	default:
		return managementJSONStatus(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// Serialize persisted mutations and restore the previous state if the write fails.
func mutateState(change func(*pluginState) error) error {
	loadState()
	dataMu.Lock()
	defer dataMu.Unlock()
	stateMu.Lock()
	before := state
	state.Keys = append([]apiKey(nil), state.Keys...)
	state.Models = append([]modelMapping(nil), state.Models...)
	state.Catalog = append([]string(nil), state.Catalog...)
	if err := change(&state); err != nil {
		state = before
		stateMu.Unlock()
		return err
	}
	// Hold the state lock through the atomic rename so readers cannot see failed writes.
	raw, err := json.MarshalIndent(state, "", "  ")
	if err == nil {
		err = writeStateFile(raw)
	}
	if err != nil {
		state = before
	}
	stateMu.Unlock()
	return err
}

func mutationError(err error) ([]byte, error) {
	var e *statusError
	if errors.As(err, &e) {
		return managementJSONStatus(e.Status, map[string]string{"error": e.Message})
	}
	return managementJSONStatus(500, map[string]string{"error": "保存失败，请检查 CPA 数据目录是否可写"})
}

func saveKey(body []byte, editing bool) ([]byte, error) {
	var input apiKey
	if json.Unmarshal(body, &input) != nil {
		return managementJSONStatus(400, map[string]string{"error": "凭证参数无效"})
	}
	input.Secret = strings.TrimSpace(input.Secret)
	input.Label = strings.TrimSpace(input.Label)
	input.BaseURL = strings.TrimSpace(input.BaseURL)
	input.Channel = strings.ToLower(strings.TrimSpace(input.Channel))
	if input.Channel != "" && input.Channel != "auto" && input.Channel != "vibe" && input.Channel != "studio" {
		return managementJSONStatus(400, map[string]string{"error": "渠道必须为 auto、vibe 或 studio"})
	}
	// Source and budget data are supplied only by Mistral, never by the client.
	input.Source, input.Quota, input.Quotas = "", nil, nil
	if input.BaseURL == "" {
		input.BaseURL = defaultBaseURL
	}
	if _, err := completionURL(input.BaseURL); err != nil {
		return managementJSONStatus(400, map[string]string{"error": "API 地址必须为有效的 HTTP(S) 地址"})
	}
	if input.Priority < -1000 || input.Priority > 1000 {
		return managementJSONStatus(400, map[string]string{"error": "优先级范围为 -1000 到 1000"})
	}
	if input.Label == "" {
		input.Label = "Mistral Key"
	}
	err := mutateState(func(s *pluginState) error {
		index := -1
		for i, key := range s.Keys {
			if editing && key.ID == input.ID {
				index = i
			}
			if input.Secret != "" && key.Secret == input.Secret && key.BaseURL == input.BaseURL && (!editing || key.ID != input.ID) {
				return &statusError{409, "该密钥已存在，请直接编辑已有凭证"}
			}
		}
		if editing {
			if index < 0 {
				return &statusError{404, "凭证不存在"}
			}
			old := s.Keys[index]
			migrateKeyQuota(&old)
			if input.Channel == "" {
				input.Channel = old.Channel
			}
			if input.Secret == "" {
				input.Secret = old.Secret
			}
			input.CreatedAt = old.CreatedAt
			if input.Secret == old.Secret && input.BaseURL == old.BaseURL {
				input.Quota = old.Quota
				input.Quotas = old.Quotas
				input.Source = keySource(old)
				input.Quota = selectedQuota(input)
			}
			s.Keys[index] = input
		} else {
			if input.Secret == "" {
				return &statusError{400, "请输入 API Key"}
			}
			input.ID = randomID()
			input.Enabled = true
			input.Quota = nil
			input.CreatedAt = time.Now().UTC()
			s.Keys = append(s.Keys, input)
		}
		return nil
	})
	if err != nil {
		return mutationError(err)
	}
	return managementJSON(publicKey(input))
}

func switchKeyChannel(body []byte) ([]byte, error) {
	var input struct {
		ID      string `json:"id"`
		Channel string `json:"channel"`
	}
	if json.Unmarshal(body, &input) != nil || input.ID == "" || (input.Channel != "auto" && input.Channel != "vibe" && input.Channel != "studio") {
		return managementJSONStatus(400, map[string]string{"error": "请选择有效的凭证和渠道"})
	}
	var key apiKey
	if err := mutateState(func(s *pluginState) error {
		for i := range s.Keys {
			if s.Keys[i].ID == input.ID {
				migrateKeyQuota(&s.Keys[i])
				s.Keys[i].Channel = input.Channel
				s.Keys[i].Quota = selectedQuota(s.Keys[i])
				key = s.Keys[i]
				return nil
			}
		}
		return &statusError{404, "凭证不存在"}
	}); err != nil {
		return mutationError(err)
	}
	return managementJSON(publicKey(key))
}

func deleteKey(id string) ([]byte, error) {
	err := mutateState(func(s *pluginState) error {
		index := -1
		for i, key := range s.Keys {
			if key.ID == id {
				index = i
				break
			}
		}
		if index < 0 {
			return &statusError{404, "凭证不存在"}
		}
		s.Keys = append(s.Keys[:index], s.Keys[index+1:]...)
		for i := range s.Models {
			if s.Models[i].KeyID == id {
				s.Models[i].KeyID = ""
			}
		}
		return nil
	})
	if err != nil {
		return mutationError(err)
	}
	return managementJSON(publicState())
}

func toggleKey(id string, enabled *bool) ([]byte, error) {
	err := mutateState(func(s *pluginState) error {
		for i := range s.Keys {
			if s.Keys[i].ID == id {
				s.Keys[i].Enabled = !s.Keys[i].Enabled
				if enabled != nil {
					s.Keys[i].Enabled = *enabled
				}
				return nil
			}
		}
		return &statusError{404, "凭证不存在"}
	})
	if err != nil {
		return mutationError(err)
	}
	return managementJSON(publicState())
}

func testKey(id, callbackID string) ([]byte, error) {
	key, ok := getKey(id)
	if !ok {
		return managementJSONStatus(http.StatusNotFound, map[string]string{"error": "key not found"})
	}
	models, err := fetchModels(key, callbackID)
	if err != nil {
		return managementJSONStatus(http.StatusBadGateway, map[string]string{"error": "Mistral API key check failed"})
	}
	return managementJSON(map[string]any{"ok": true, "model_count": len(models)})
}

func syncModels(body []byte, callbackID string) ([]byte, error) {
	var input struct {
		KeyID string `json:"key_id"`
	}
	if err := json.Unmarshal(body, &input); err != nil || input.KeyID == "" {
		return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": "key_id is required"})
	}
	key, ok := getKey(input.KeyID)
	if !ok {
		return managementJSONStatus(http.StatusNotFound, map[string]string{"error": "key not found"})
	}
	models, err := fetchModels(key, callbackID)
	if err != nil {
		return managementJSONStatus(http.StatusBadGateway, map[string]string{"error": "model list request failed"})
	}
	if err := mutateState(func(s *pluginState) error { s.Catalog = models; return nil }); err != nil {
		return mutationError(err)
	}
	return managementJSON(publicState())
}

func modelRevision(models []modelMapping) string {
	raw, _ := json.Marshal(models)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:12])
}

func saveMapping(body []byte) ([]byte, error) {
	var model modelMapping
	if json.Unmarshal(body, &model) != nil {
		return managementJSONStatus(400, map[string]string{"error": "模型映射参数无效"})
	}
	model.ClientModel = strings.TrimPrefix(strings.TrimSpace(model.ClientModel), pluginID+"/")
	model.UpstreamModel = strings.TrimSpace(model.UpstreamModel)
	if model.ClientModel == "" || model.UpstreamModel == "" {
		return managementJSONStatus(400, map[string]string{"error": "请填写请求模型与上游模型"})
	}
	err := mutateState(func(s *pluginState) error {
		index := -1
		for i, existing := range s.Models {
			if existing.ID == model.ID {
				index = i
			}
			if existing.ClientModel == model.ClientModel && existing.ID != model.ID {
				return &statusError{409, "请求模型名称已存在"}
			}
		}
		if model.KeyID != "" {
			found := false
			for _, key := range s.Keys {
				if key.ID == model.KeyID {
					found = true
				}
			}
			if !found {
				return &statusError{400, "绑定凭证不存在"}
			}
		}
		if model.ID == "" {
			model.ID = randomID()
			s.Models = append(s.Models, model)
		} else {
			if index < 0 {
				return &statusError{404, "模型映射不存在"}
			}
			s.Models[index] = model
		}
		return nil
	})
	if err != nil {
		return mutationError(err)
	}
	return managementJSON(publicState())
}

func deleteMapping(id string) ([]byte, error) {
	err := mutateState(func(s *pluginState) error {
		for i, model := range s.Models {
			if model.ID == id {
				s.Models = append(s.Models[:i], s.Models[i+1:]...)
				return nil
			}
		}
		return &statusError{404, "模型映射不存在"}
	})
	if err != nil {
		return mutationError(err)
	}
	return managementJSON(publicState())
}

func updateMappings(body []byte) ([]byte, error) {
	var input struct {
		IDs     []string `json:"ids"`
		Enabled bool     `json:"enabled"`
	}
	if json.Unmarshal(body, &input) != nil || len(input.IDs) == 0 {
		return managementJSONStatus(400, map[string]string{"error": "请选择模型映射"})
	}
	err := mutateState(func(s *pluginState) error {
		selected := make(map[string]bool, len(input.IDs))
		for _, id := range input.IDs {
			selected[id] = true
		}
		for i := range s.Models {
			if selected[s.Models[i].ID] {
				s.Models[i].Enabled = input.Enabled
			}
		}
		return nil
	})
	if err != nil {
		return mutationError(err)
	}
	return managementJSON(publicState())
}

func fetchModels(key apiKey, callbackID string) ([]string, error) {
	base := key.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/models"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	raw, err := doUpstreamHTTP(nil, upstreamRequest{
		Method: http.MethodGet, URL: parsed.String(),
		Headers:        http.Header{"Authorization": []string{"Bearer " + key.Secret}, "Accept": []string{"application/json"}},
		HostCallbackID: callbackID,
	}, currentState().ProxyURL)
	if err != nil {
		return nil, err
	}
	var response upstreamResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("model catalog returned status %d", response.StatusCode)
	}
	return parseChatCatalog(response.Body)
}

func parseChatCatalog(body []byte) ([]string, error) {
	var catalog struct {
		Data []struct {
			ID           string          `json:"id"`
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(catalog.Data))
	hasChatCapability := false
	for _, item := range catalog.Data {
		if _, hasCapability := item.Capabilities["completion_chat"]; hasCapability {
			hasChatCapability = true
		}
		if id := strings.TrimSpace(item.ID); id != "" && item.Capabilities["completion_chat"] {
			models = append(models, id)
		}
	}
	if !hasChatCapability {
		for _, item := range catalog.Data {
			if id := strings.TrimSpace(item.ID); id != "" {
				models = append(models, id)
			}
		}
	}
	sort.Strings(models)
	if len(models) == 0 {
		return nil, errors.New("model catalog is empty")
	}
	return models, nil
}

func getKey(id string) (apiKey, bool) {
	stateMu.RLock()
	defer stateMu.RUnlock()
	for _, key := range state.Keys {
		if key.ID == id {
			return key, true
		}
	}
	return apiKey{}, false
}

func resolveKeyAndModel(snapshot pluginState, requested string) (apiKey, modelMapping, bool) {
	modelName := strings.TrimPrefix(requested, pluginID+"/")
	var model *modelMapping
	for i := range snapshot.Models {
		if snapshot.Models[i].Enabled && snapshot.Models[i].ClientModel == modelName {
			model = &snapshot.Models[i]
			break
		}
	}
	if model == nil {
		return apiKey{}, modelMapping{}, false
	}
	keys := make([]apiKey, 0, len(snapshot.Keys))
	for _, key := range snapshot.Keys {
		if key.Enabled && (model.KeyID == "" || key.ID == model.KeyID) {
			if len(keys) > 0 && key.Priority < keys[0].Priority {
				continue
			}
			if len(keys) > 0 && key.Priority > keys[0].Priority {
				keys = keys[:0]
			}
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return apiKey{}, modelMapping{}, false
	}
	index := keyTurn.Add(1) - 1
	return keys[index%uint64(len(keys))], *model, true
}

func currentState() pluginState {
	loadState()
	stateMu.RLock()
	defer stateMu.RUnlock()
	return pluginState{
		Keys:      append([]apiKey(nil), state.Keys...),
		Catalog:   append([]string{}, state.Catalog...),
		Models:    append([]modelMapping{}, state.Models...),
		DebugMode: state.DebugMode,
		ProxyURL:  state.ProxyURL,
		ImageLimit: state.ImageLimit,
		ConnectorMode: state.ConnectorMode,
	}
}

func publicState() map[string]any {
	snapshot := currentState()
	keys := make([]safeKey, 0, len(snapshot.Keys))
	for _, key := range snapshot.Keys {
		keys = append(keys, publicKey(key))
	}
	stateMu.RLock()
	published := publishedRevision
	stateMu.RUnlock()
	return map[string]any{"keys": keys, "catalog": snapshot.Catalog, "models": snapshot.Models, "debug_mode": snapshot.DebugMode, "proxy_url": snapshot.ProxyURL, "connector_mode": connectorModeOf(snapshot), "models_revision": modelRevision(snapshot.Models), "published_revision": published}
}

func publicKey(key apiKey) safeKey {
	last := key.Secret
	if len(last) > 4 {
		last = last[len(last)-4:]
	}
	channel := key.Channel
	if channel == "" {
		channel = "auto"
	}
	return safeKey{ID: key.ID, Label: key.Label, Masked: "••••" + last, BaseURL: key.BaseURL, Enabled: key.Enabled, Priority: key.Priority, Channel: channel, Source: keySource(key), ActiveChannel: activeChannel(key), Quotas: key.Quotas, Quota: selectedQuota(key), CreatedAt: key.CreatedAt}
}

func managementJSON(value any) ([]byte, error) {
	return managementJSONStatus(http.StatusOK, value)
}

func managementJSONStatus(status int, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return managementResult(status, body)
}

func managementPage(body []byte) ([]byte, error) {
	return okEnvelope(managementResponse{StatusCode: http.StatusOK, Headers: http.Header{"content-type": []string{"text/html; charset=utf-8"}}, Body: body})
}

func managementResult(status int, body []byte) ([]byte, error) {
	return okEnvelope(managementResponse{StatusCode: status, Headers: http.Header{"content-type": []string{"application/json; charset=utf-8"}}, Body: body})
}

func randomID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes[:])
}

func appendRequestLog(entry RequestLog) error {
	if entry.ID == "" {
		entry.ID = randomID()
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	logMu.Lock()
	defer logMu.Unlock()
	stateMu.Lock()
	state.Logs = append(state.Logs, entry)
	if excess := len(state.Logs) - maxRequestLogs; excess > 0 {
		state.Logs = state.Logs[excess:]
	}
	stateMu.Unlock()
	path, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(logPath(path), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if info.Size() > 16*1024*1024 {
		return compactRequestLogs(path)
	}
	return nil
}

func compactRequestLogs(stateFile string) error {
	stateMu.RLock()
	logs := append([]RequestLog(nil), state.Logs...)
	stateMu.RUnlock()
	raw := make([]byte, 0, len(logs)*160)
	for _, entry := range logs {
		line, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		raw = append(raw, line...)
		raw = append(raw, '\n')
	}
	path := logPath(stateFile)
	temp := path + ".tmp"
	if err := os.WriteFile(temp, raw, 0600); err != nil {
		return err
	}
	if err := os.Chmod(temp, 0600); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func clearRequestLogs() error {
	loadState()
	if err := clearDebugLogs(); err != nil {
		return err
	}
	logMu.Lock()
	defer logMu.Unlock()
	stateMu.Lock()
	state.Logs = []RequestLog{}
	stateMu.Unlock()
	path, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(logPath(path), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return file.Close()
}

func readRequestLogs(stateFile string) []RequestLog {
	file, err := os.Open(logPath(stateFile))
	if err != nil {
		return []RequestLog{}
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	logs := make([]RequestLog, 0, maxRequestLogs)
	for scanner.Scan() {
		var entry RequestLog
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			continue
		}
		logs = append(logs, entry)
		if excess := len(logs) - maxRequestLogs; excess > 0 {
			logs = logs[excess:]
		}
	}
	return logs
}

func logPath(stateFile string) string {
	return filepath.Join(filepath.Dir(stateFile), "requests.jsonl")
}

func loadState() {
	dataMu.Lock()
	defer dataMu.Unlock()
	if stateLoaded {
		return
	}
	path, err := statePath()
	if err != nil {
		stateLoaded = true
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		stateLoaded = true
		return
	}
	var stored pluginState
	if json.Unmarshal(raw, &stored) != nil {
		stateLoaded = true
		return
	}
	if stored.Keys == nil {
		stored.Keys = []apiKey{}
	}
	if stored.Catalog == nil {
		stored.Catalog = []string{}
	}
	if stored.Models == nil {
		stored.Models = []modelMapping{}
	}
	for i := range stored.Models {
		stored.Models[i].ClientModel = strings.TrimPrefix(stored.Models[i].ClientModel, pluginID+"/")
	}
	stored.Logs = readRequestLogs(path)
	stateMu.Lock()
	state = stored
	stateMu.Unlock()
	stateLoaded = true
}

func writeStateFile(raw []byte) error {
	path, err := statePath()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temp := path + ".tmp"
	if err = os.WriteFile(temp, raw, 0600); err != nil {
		return err
	}
	if err = os.Chmod(temp, 0600); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}

func statePath() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("MISTRAL_BRIDGE_DATA_DIR")); configured != "" {
		return filepath.Join(configured, "state.json"), nil
	}
	if configured := strings.TrimSpace(dataDir); configured != "" {
		return filepath.Join(configured, "plugin-data", pluginID, "state.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cli-proxy-api", "plugin-data", pluginID, "state.json"), nil
}

func configureDataDirectory(authDir string) {
	if strings.TrimSpace(authDir) == "" {
		return
	}
	dataMu.Lock()
	if !stateLoaded {
		dataDir = authDir
	}
	dataMu.Unlock()
}


// connector mode: "off" disables all connector features, "directive" (default)
// executes only explicit x_vibe_connector directives, "loop" additionally
// injects connector tools and auto-executes them (multi-round).
func connectorModeOf(st pluginState) string {
	switch st.ConnectorMode {
	case "off", "loop":
		return st.ConnectorMode
	default:
		return "directive"
	}
}

func updateConnectorMode(body []byte) ([]byte, error) {
	var input struct {
		Mode string `json:"connector_mode"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": "invalid connector_mode payload"})
	}
	mode := strings.ToLower(strings.TrimSpace(input.Mode))
	if mode != "off" && mode != "directive" && mode != "loop" {
		return managementJSONStatus(http.StatusBadRequest, map[string]string{"error": "connector_mode must be off, directive or loop"})
	}
	if err := mutateState(func(s *pluginState) error { s.ConnectorMode = mode; return nil }); err != nil {
		return managementJSONStatus(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return managementJSON(publicState())
}
