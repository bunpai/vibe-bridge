// connector.go — Mistral connectors (cloud MCP) client-side execution.
// Two entry points:
//   - executeConnectorDirective: the "x_vibe_connector" request field runs one
//     connector tool and returns a synthesized chat completion (client-key auth,
//     no upstream LLM call).
//   - injectConnectorTools + runConnectorLoop: model-visible connector tools,
//     intercepted tool_calls executed host-side (works for any OpenAI client).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const connectorBootstrapQuery = "?include_auth_actionable_connectors=true&builtin_connectors=web_search&supports_mcp=true"

type connectorState struct {
	id          string // web_search connector id
	initialized bool
}
type connectorKey struct {
	mu    sync.Mutex
	byKey map[string]*connectorState
}

var connectorCache = &connectorKey{byKey: map[string]*connectorState{}}

func connectorHeaders(key apiKey) http.Header {
	h := http.Header{
		"Authorization": []string{"Bearer " + key.Secret},
		"Content-Type":  []string{"application/json"},
		"Accept":        []string{"application/json, text/event-stream"},
		"User-Agent":    []string{vibeUserAgent},
	}
	return h
}

// connectorHTTP issues one upstream HTTP call through the host bridge and
// returns status + parsed body (JSON or first SSE data event).
func connectorHTTP(ctx context.Context, key apiKey, callbackID, method, path string, body any) (int, json.RawMessage, error) {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	base := strings.TrimRight(key.BaseURL, "/")
	req := upstreamRequest{Method: method, URL: base + path, Headers: connectorHeaders(key), Body: payload, HostCallbackID: callbackID}
	raw, err := doUpstreamHTTP(ctx, req, currentState().ProxyURL)
	if err != nil {
		return 0, nil, fmt.Errorf("connector http: %w", err)
	}
	var resp upstreamResponse
	if err = json.Unmarshal(raw, &resp); err != nil {
		return 0, nil, fmt.Errorf("connector response decode: %w", err)
	}
	return resp.StatusCode, parseRPCBody(resp.Body), nil
}

// parseRPCBody accepts either a plain JSON body or an SSE stream and returns
// the first JSON-RPC payload found.
func parseRPCBody(body []byte) json.RawMessage {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if line == "[DONE]" || line == "" {
				continue
			}
			return json.RawMessage(line)
		}
	}
	return json.RawMessage(strings.TrimSpace(string(body)))
}

// connectorID resolves (and caches) the web_search connector id via bootstrap.
func connectorID(ctx context.Context, key apiKey, callbackID string) (string, error) {
	if st := connectorCache.byKey[key.ID]; st != nil && st.id != "" {
		return st.id, nil
	}
	status, raw, err := connectorHTTP(ctx, key, callbackID, http.MethodGet, "/connectors/bootstrap"+connectorBootstrapQuery, nil)
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("connector bootstrap status %d: %s", status, string(raw)[:200])
	}
	var boot struct {
		Connectors []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status struct {
				IsReady bool `json:"is_ready"`
			} `json:"status"`
		} `json:"connectors"`
	}
	if err = json.Unmarshal(raw, &boot); err != nil {
		return "", fmt.Errorf("connector bootstrap decode: %w", err)
	}
	for _, c := range boot.Connectors {
		if c.Name == "web_search" && c.Status.IsReady {
			connectorCache.byKey[key.ID] = &connectorState{id: c.ID}
			return c.ID, nil
		}
	}
	return "", fmt.Errorf("web_search connector not available on this key")
}

// executeConnectorTool runs initialize → initialized → tools/call and returns
// the result content as a string (the JSON-RPC result serialized).
func executeConnectorTool(ctx context.Context, key apiKey, callbackID, tool string, args map[string]any) (string, error) {
	cid, err := connectorID(ctx, key, callbackID)
	if err != nil {
		return "", err
	}
	mcp := "/connectors-gateway/" + cid + "/mcp"
	// initialize (stateless gateway — session headers optional)
	_, _, err = connectorHTTP(ctx, key, callbackID, http.MethodPost, mcp,
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
				"clientInfo": map[string]any{"name": "vibe-bridge", "version": "1.0.0"}}})
	if err != nil {
		return "", fmt.Errorf("mcp initialize: %w", err)
	}
	_, _, _ = connectorHTTP(ctx, key, callbackID, http.MethodPost, mcp,
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	// tools/call
	status, raw, err := connectorHTTP(ctx, key, callbackID, http.MethodPost, mcp,
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		return "", fmt.Errorf("mcp tools/call: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("mcp tools/call status %d: %s", status, string(raw)[:300])
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err = json.Unmarshal(raw, &rpc); err != nil {
		return "", fmt.Errorf("mcp result decode: %w", err)
	}
	if rpc.Error != nil {
		return "", fmt.Errorf("mcp error %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	// result.content[0].text holds the JSON-typed result; return raw result JSON
	if rpc.Result != nil {
		return string(rpc.Result), nil
	}
	return "{}", nil
}

// ---------- directive (B): explicit connector execution via x_vibe_connector ----------

type connectorDirective struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

func parseConnectorDirective(raw []byte) *connectorDirective {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	f, ok := root["x_vibe_connector"]
	if !ok {
		return nil
	}
	var d connectorDirective
	if err := json.Unmarshal(f, &d); err != nil || d.Tool == "" {
		return nil
	}
	return &d
}

// stripConnectorFields removes x_vibe_* request fields before upstream send.
func stripConnectorFields(raw []byte) []byte {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw
	}
	delete(root, "x_vibe_connector")
	delete(root, "x_vibe_connectors")
	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
}

// synthChatCompletion wraps a connector result in a standard chat.completion.
func synthChatCompletion(model, content string) []byte {
	resp := map[string]any{
		"id":      "chatcmpl-vibe-connector",
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
	b, _ := json.Marshal(resp)
	return b
}

// emitSynthesizedStream pushes a one-shot SSE reply through the downstream
// stream bridge for stream:true connector-directive requests.
func emitSynthesizedStream(req rpcExecutorRequest, content string) {
	chunk := map[string]any{
		"id": "chatcmpl-vibe-connector", "object": "chat.completion.chunk", "created": 0, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}, "finish_reason": nil}},
	}
	b, _ := json.Marshal(chunk)
	hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": []byte("data: " + string(b) + "\n\n")})
	fin := map[string]any{
		"id": "chatcmpl-vibe-connector", "object": "chat.completion.chunk", "created": 0, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}
	fb, _ := json.Marshal(fin)
	hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": []byte("data: " + string(fb) + "\n\ndata: [DONE]\n\n")})
	hostCall("host.stream.close", map[string]any{"stream_id": req.StreamID})
}

// ---------- auto-loop (A): inject connector tools + execute their calls ----------

// connectorToolDef builds an OpenAI function tool definition the model can call.
func connectorToolDef(name string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "connector_web_search." + name,
			"description": "Search the web via the Mistral web_search connector (server-side MCP).",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": map[string]any{"type": "string", "description": "search query"}},
				"required":   []string{"query"},
			},
		},
	})
	return b
}

// wantsConnectorTools reports whether the request opted into connector tool
// injection via x_vibe_connectors (true or a list of tool names).
func wantsConnectorTools(raw []byte) ([]string, bool) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false
	}
	f, ok := root["x_vibe_connectors"]
	if !ok {
		return nil, false
	}
	var list []string
	if err := json.Unmarshal(f, &list); err == nil && len(list) > 0 {
		return list, true
	}
	var on bool
	if err := json.Unmarshal(f, &on); err == nil && on {
		return []string{"web_search"}, true
	}
	return nil, false
}

func injectConnectorTools(body []byte, names []string) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body, err
	}
	var tools []json.RawMessage
	if t, ok := root["tools"]; ok && len(t) > 0 && string(t) != "null" {
		if err := json.Unmarshal(t, &tools); err != nil {
			tools = nil
		}
	}
	for _, n := range names {
		tools = append(tools, connectorToolDef(n))
	}
	b, _ := json.Marshal(tools)
	root["tools"] = b
	out, err := json.Marshal(root)
	if err != nil {
		return body, err
	}
	return out, nil
}

type connectorCall struct {
	ID   string
	Name string
	Args map[string]any
}

// extractConnectorCalls parses a chat.completion body: returns the assistant
// message and all tool_calls whose name targets a connector (connector_*.).
func extractConnectorCalls(result []byte) (json.RawMessage, []connectorCall, error) {
	var root struct {
		Choices []struct {
			Message struct {
				Raw       json.RawMessage `json:"-"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(result, &root); err != nil {
		return nil, nil, err
	}
	if len(root.Choices) == 0 {
		return nil, nil, nil
	}
	// re-unmarshal assistant message raw
	var msgObj map[string]json.RawMessage
	var choices []map[string]json.RawMessage
	_ = json.Unmarshal(result, &struct{ Choices *[]map[string]json.RawMessage }{&choices})
	if len(choices) > 0 {
		_ = json.Unmarshal(choices[0]["message"], &msgObj)
	}
	// upstream rejects reasoning fields on replayed assistant messages
	delete(msgObj, "reasoning_content")
	delete(msgObj, "reasoning")
	raw, _ := json.Marshal(msgObj)
	var calls []connectorCall
	for _, tc := range root.Choices[0].Message.ToolCalls {
		if !strings.HasPrefix(tc.Function.Name, "connector_") {
			continue
		}
		var args map[string]any
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		tool := tc.Function.Name
		if i := strings.LastIndex(tool, "."); i >= 0 {
			tool = tool[i+1:]
		}
		calls = append(calls, connectorCall{ID: tc.ID, Name: tool, Args: args})
	}
	return raw, calls, nil
}

// appendConnectorResults adds the assistant tool_call message plus tool results
// to the request body's messages array.
func appendConnectorResults(body []byte, assistant json.RawMessage, results []connectorResult) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body, err
	}
	var messages []json.RawMessage
	_ = json.Unmarshal(root["messages"], &messages)
	messages = append(messages, assistant)
	for _, r := range results {
		m, _ := json.Marshal(map[string]any{
			"role": "tool", "tool_call_id": r.id, "content": r.content,
		})
		messages = append(messages, m)
	}
	b, _ := json.Marshal(messages)
	root["messages"] = b
	out, err := json.Marshal(root)
	return out, err
}

type connectorResult struct {
	id      string
	content string
}
