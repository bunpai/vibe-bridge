package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

const defaultBaseURL = "https://api.mistral.ai/v1"

// vibeClientVersion mirrors the official Mistral Vibe CLI release whose request
// shape we reproduce; bump it when Mistral changes the fingerprint.
const vibeClientVersion = "3.1.4"

// vibeUserAgent is the User-Agent official Vibe sends on the mistral backend
// (mistral-client-python SDK prefix + Mistral-Vibe/version).
const vibeUserAgent = "mistral-client-python/Mistral-Vibe/" + vibeClientVersion

// vibeSessionID is one CPA process's Vibe session identity: official Vibe mints
// a session id per CLI launch, so the bridge mints one per process lifetime.
// It doubles as the x-affinity cache-routing token.
var vibeSessionID = newVibeSessionID()

// vibeOSVersion mirrors Vibe's telemetry os_version (a kernel/OS release string).
var vibeOSVersion = detectOSVersion()

// newVibeSessionID returns a random UUIDv4 string without new dependencies.
func newVibeSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return randomID()
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// detectOSVersion runs once at startup; empty when uname is unavailable.
func detectOSVersion() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type rpcExecutorRequest struct {
	Model          string         `json:"Model"`
	Payload        []byte         `json:"Payload"`
	Stream         bool           `json:"Stream"`
	Headers        http.Header    `json:"Headers"`
	Query          url.Values     `json:"Query"`
	Metadata       map[string]any `json:"Metadata"`
	HostCallbackID string         `json:"host_callback_id,omitempty"`
	StreamID       string         `json:"stream_id,omitempty"`
}

type upstreamRequest struct {
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers"`
	Body           []byte      `json:"body,omitempty"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type upstreamResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

type upstreamStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
	Body       []byte      `json:"body,omitempty"`
	reader     io.ReadCloser
	cleanup    func()
}

type streamReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

type statusError struct {
	Status  int
	Message string
}

func (e *statusError) Error() string { return e.Message }

func performCompletion(ctx context.Context, req rpcExecutorRequest, trace *requestTrace) ([]byte, http.Header, error) {
	settings := currentState()
	key, model, ok := resolveKeyAndModel(settings, req.Model)
	if !ok {
		return nil, nil, &statusError{Status: 404, Message: "Mistral model mapping was not found"}
	}
	trace.resolved(key, model, req.Payload)
	if dir := parseConnectorDirective(req.Payload); dir != nil {
		result, errDir := executeConnectorTool(ctx, key, req.HostCallbackID, dir.Tool, dir.Args)
		if errDir != nil {
			return nil, nil, &statusError{Status: 502, Message: "Connector execution failed: " + errDir.Error()}
		}
		trace.completeResponse()
		return synthChatCompletion(req.Model, result), http.Header{"Content-Type": []string{"application/json"}}, nil
	}
	payload := stripConnectorFields(req.Payload)
	body, err := transformRequest(payload, model.UpstreamModel, model.ThinkingMap, imageLimitFor(settings, model))
	if err == nil {
		body, err = applyKeyChannel(body, key)
	}
	if err != nil {
		return nil, nil, &statusError{Status: 400, Message: "Invalid chat completion request"}
	}
	if names, wantTools := wantsConnectorTools(req.Payload); wantTools {
		body, err = injectConnectorTools(body, names)
		if err != nil {
			return nil, nil, &statusError{Status: 400, Message: "Invalid connector tool injection"}
		}
	}
	if req.Stream {
		body, err = setJSONField(body, "stream", true)
		if err == nil {
			body, err = ensureStreamUsage(body)
		}
	} else {
		body, err = setJSONField(body, "stream", false)
	}
	if err != nil {
		return nil, nil, &statusError{Status: 400, Message: "Invalid chat completion request"}
	}
	trace.resolved(key, model, body)
	endpoint, err := completionURL(key.BaseURL)
	if err != nil {
		return nil, nil, &statusError{Status: 500, Message: "Invalid Mistral API endpoint"}
	}
	headers := http.Header{"Authorization": []string{"Bearer " + key.Secret}, "Content-Type": []string{"application/json"}, "Accept": []string{"application/json"}}
	if req.Stream {
		headers.Set("Accept", "text/event-stream")
	}
	for name, value := range vibeRequestHeaders(key) {
		headers.Set(name, value)
	}
	callbackReq := upstreamRequest{Method: http.MethodPost, URL: endpoint, Headers: headers, Body: body, HostCallbackID: req.HostCallbackID}
	trace.captureRequest(callbackReq.Method, callbackReq.URL, callbackReq.Headers, callbackReq.Body)
	responseRaw, err := doUpstreamHTTP(ctx, callbackReq, settings.ProxyURL)
	if err != nil {
		trace.captureResponseError(err)
		return nil, nil, &statusError{Status: 502, Message: "Mistral API request failed"}
	}
	var response upstreamResponse
	if err = json.Unmarshal(responseRaw, &response); err != nil {
		trace.captureResponse(0, nil, responseRaw, true)
		return nil, nil, &statusError{Status: 502, Message: "Invalid response from HTTP bridge"}
	}
	trace.captureResponse(response.StatusCode, response.Headers, response.Body, true)
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, nil, upstreamStatusError(response.StatusCode, response.Body)
	}
	result, err := normalizeCompletion(response.Body)
	if err == nil && !req.Stream {
		if _, wantLoop := wantsConnectorTools(req.Payload); wantLoop {
			for round := 0; round < 4; round++ {
				assistant, calls, e := extractConnectorCalls(result)
				if e != nil || len(calls) == 0 {
					break
				}
				results := make([]connectorResult, 0, len(calls))
				for _, c := range calls {
					content, cerr := executeConnectorTool(ctx, key, req.HostCallbackID, c.Name, c.Args)
					if cerr != nil {
						content = "connector error: " + cerr.Error()
					}
					results = append(results, connectorResult{id: c.ID, content: content})
				}
				if body, err = appendConnectorResults(body, assistant, results); err != nil {
					break
				}
				callbackReq.Body = body
				trace.captureRequest(callbackReq.Method, callbackReq.URL, callbackReq.Headers, callbackReq.Body)
				responseRaw, err = doUpstreamHTTP(ctx, callbackReq, settings.ProxyURL)
				if err != nil {
					break
				}
				if err = json.Unmarshal(responseRaw, &response); err != nil {
					break
				}
				trace.captureResponse(response.StatusCode, response.Headers, response.Body, true)
				if response.StatusCode < 200 || response.StatusCode > 299 {
					return nil, nil, upstreamStatusError(response.StatusCode, response.Body)
				}
				if result, err = normalizeCompletion(response.Body); err != nil {
					break
				}
			}
		}
	}
	return result, response.Headers, err
}

func performCompletionStream(ctx context.Context, req rpcExecutorRequest, trace *requestTrace) (http.Header, error) {
	settings := currentState()
	key, model, ok := resolveKeyAndModel(settings, req.Model)
	if !ok {
		return nil, &statusError{Status: 404, Message: "Mistral model mapping was not found"}
	}
	trace.resolved(key, model, req.Payload)
	if dir := parseConnectorDirective(req.Payload); dir != nil {
		go func() {
			result, errDir := executeConnectorTool(ctx, key, req.HostCallbackID, dir.Tool, dir.Args)
			if errDir != nil {
				hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "error": "connector: " + errDir.Error()})
				hostCall("host.stream.close", map[string]any{"stream_id": req.StreamID})
				return
			}
			emitSynthesizedStream(req, result)
		}()
		return http.Header{"content-type": []string{"text/event-stream"}}, nil
	}
	payload := stripConnectorFields(req.Payload)
	body, err := transformRequest(payload, model.UpstreamModel, model.ThinkingMap, imageLimitFor(settings, model))
	if err == nil {
		body, err = applyKeyChannel(body, key)
	}
	if err == nil {
		body, err = setJSONField(body, "stream", true)
	}
	if err == nil {
		body, err = ensureStreamUsage(body)
	}
	if err != nil {
		return nil, &statusError{Status: 400, Message: "Invalid chat completion request"}
	}
	trace.resolved(key, model, body)
	endpoint, err := completionURL(key.BaseURL)
	if err != nil {
		return nil, &statusError{Status: 500, Message: "Invalid Mistral API endpoint"}
	}
	headers := http.Header{"Authorization": []string{"Bearer " + key.Secret}, "Content-Type": []string{"application/json"}, "Accept": []string{"text/event-stream"}}
	for name, value := range vibeRequestHeaders(key) {
		headers.Set(name, value)
	}
	trace.captureRequest(http.MethodPost, endpoint, headers, body)
	stream, err := openUpstreamHTTPStream(ctx, upstreamRequest{
		Method: http.MethodPost, URL: endpoint, Headers: headers,
		Body: body, HostCallbackID: req.HostCallbackID,
	}, settings.ProxyURL)
	if err != nil {
		trace.captureResponseError(err)
		return nil, &statusError{Status: 502, Message: "Mistral streaming request failed"}
	}
	trace.captureResponseHead(stream.StatusCode, stream.Headers)
	if stream.StatusCode < 200 || stream.StatusCode > 299 {
		body := append([]byte(nil), stream.Body...)
		trace.captureResponseChunk(stream.Body)
		if stream.opened() {
			if trace.capturesDebug() || stream.reader != nil {
				body = append(body, readStreamErrorBody(req, stream, trace)...)
			} else {
				stream.close(req.HostCallbackID)
			}
		} else {
			trace.completeResponse()
		}
		return nil, upstreamStatusError(stream.StatusCode, body)
	}
	if !stream.opened() {
		trace.captureResponseError(errors.New("Mistral stream was not opened"))
		return nil, &statusError{Status: 502, Message: "Mistral stream was not opened"}
	}
	go relayStream(req, stream, trace)
	return http.Header{"content-type": []string{"text/event-stream"}}, nil
}

func readStreamErrorBody(req rpcExecutorRequest, stream upstreamStreamResponse, trace *requestTrace) []byte {
	defer stream.close(req.HostCallbackID)
	var body bytes.Buffer
	for {
		chunk, err := stream.read(req.HostCallbackID)
		if err != nil {
			trace.captureResponseError(err)
			return body.Bytes()
		}
		if chunk.Error != "" {
			trace.captureResponseError(errors.New(chunk.Error))
			return body.Bytes()
		}
		if len(chunk.Payload) > 0 {
			_, _ = body.Write(chunk.Payload)
			trace.captureResponseChunk(chunk.Payload)
		}
		if chunk.Done {
			trace.completeResponse()
			return body.Bytes()
		}
	}
}

func relayStream(req rpcExecutorRequest, stream upstreamStreamResponse, trace *requestTrace) {
	var streamErr error
	var usage map[string]int
	defer func() { trace.finish(usage, streamErr) }()
	defer stream.close(req.HostCallbackID)
	defer hostCall("host.stream.close", map[string]any{"stream_id": req.StreamID})
	var pending []byte
	for {
		chunk, err := stream.read(req.HostCallbackID)
		if err != nil {
			streamErr = &statusError{502, "Mistral 流式响应中断"}
			trace.captureResponseError(err)
			hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "error": "Mistral stream read failed"})
			return
		}
		trace.captureResponseChunk(chunk.Payload)
		if chunk.Error != "" {
			streamErr = &statusError{502, "Mistral 流式响应中断"}
			trace.captureResponseError(errors.New(chunk.Error))
			hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "error": "Mistral stream failed"})
			return
		}
		pending = append(pending, chunk.Payload...)
		for {
			lineEnd := bytes.IndexByte(pending, '\n')
			if lineEnd < 0 {
				break
			}
			line := append([]byte(nil), pending[:lineEnd+1]...)
			pending = pending[lineEnd+1:]
			if lineUsage := parseSSEUsage(line); lineUsage != nil {
				usage = lineUsage
			}
			processed, errLine := normalizeSSELine(line)
			if errLine != nil {
				streamErr = &statusError{502, "Mistral 流式响应中断"}
				trace.captureResponseError(errLine)
				hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "error": "Invalid Mistral stream event"})
				if trace.capturesDebug() {
					if err := drainUpstreamStream(req, stream, trace); err != nil {
						trace.captureResponseError(err)
					}
				}
				return
			}
			if len(processed) > 0 {
				trace.observe(processed)
				if _, errEmit := hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": processed}); errEmit != nil {
					streamErr = &statusError{499, "客户端已断开连接"}
					if !trace.capturesDebug() {
						return
					}
					if err := drainUpstreamStream(req, stream, trace); err != nil {
						trace.captureResponseError(err)
					}
					return
				}
			}
		}
		if chunk.Done {
			trace.completeResponse()
			if len(pending) > 0 {
				if lineUsage := parseSSEUsage(pending); lineUsage != nil {
					usage = lineUsage
				}
				processed, errLine := normalizeSSELine(pending)
				if errLine == nil && len(processed) > 0 {
					trace.observe(processed)
					if _, err := hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": processed}); err != nil {
						streamErr = &statusError{499, "客户端已断开连接"}
					}
				} else if errLine != nil {
					streamErr = &statusError{502, "Mistral 流式响应中断"}
					trace.captureResponseError(errLine)
				}
			}
			return
		}
	}
}

func drainUpstreamStream(req rpcExecutorRequest, stream upstreamStreamResponse, trace *requestTrace) error {
	for {
		chunk, err := stream.read(req.HostCallbackID)
		if err != nil {
			return err
		}
		if chunk.Error != "" {
			return errors.New(chunk.Error)
		}
		trace.captureResponseChunk(chunk.Payload)
		if chunk.Done {
			trace.completeResponse()
			return nil
		}
	}
}

func normalizeSSELine(line []byte) ([]byte, error) {
	trimmed := bytes.TrimSuffix(line, []byte("\n"))
	trimmed = bytes.TrimSuffix(trimmed, []byte("\r"))
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line, nil
	}
	data := bytes.TrimSpace(trimmed[len("data:"):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return line, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if err := normalizeChoices(root, true); err != nil {
		return nil, err
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("data: "), out...), '\n'), nil
}

func normalizeCompletion(body []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, &statusError{Status: 502, Message: "Mistral returned invalid JSON"}
	}
	if err := normalizeChoices(root, false); err != nil {
		return nil, &statusError{Status: 502, Message: "Mistral returned an unsupported content shape"}
	}
	return json.Marshal(root)
}

func normalizeChoices(root map[string]json.RawMessage, streaming bool) error {
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(root["choices"], &choices); err != nil {
		return err
	}
	for _, choice := range choices {
		field := "message"
		if streaming {
			field = "delta"
		}
		var message map[string]json.RawMessage
		if json.Unmarshal(choice[field], &message) != nil || message == nil {
			continue
		}
		content, exists := message["content"]
		if !exists || len(bytes.TrimSpace(content)) == 0 || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
			continue
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(content, &parts); err != nil {
			continue
		}
		var thinking, text strings.Builder
		for _, part := range parts {
			var kind string
			_ = json.Unmarshal(part["type"], &kind)
			if kind == "thinking" {
				var thoughtParts []struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(part["thinking"], &thoughtParts)
				for _, item := range thoughtParts {
					thinking.WriteString(item.Text)
				}
				continue
			}
			if kind == "text" {
				var value string
				_ = json.Unmarshal(part["text"], &value)
				text.WriteString(value)
			}
		}
		newContent, err := json.Marshal(text.String())
		if err != nil {
			return err
		}
		message["content"] = newContent
		if thinking.Len() > 0 {
			reasoning, _ := json.Marshal(thinking.String())
			message["reasoning_content"] = reasoning
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			return err
		}
		choice[field] = encoded
	}
	encoded, err := json.Marshal(choices)
	if err != nil {
		return err
	}
	root["choices"] = encoded
	return nil
}

// Mistral meters metadata.call_source=vibe_code against the Vibe allowance.
// Studio uses the upstream API route by omitting this marker, not by changing
// its value: Mistral may classify any nonempty call_source as Vibe traffic.
func applyKeyChannel(raw []byte, key apiKey) ([]byte, error) {
	channel := activeChannel(key)
	if channel == "" {
		return raw, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	metadata := map[string]json.RawMessage{}
	if value := root["metadata"]; len(value) > 0 {
		if err := json.Unmarshal(value, &metadata); err != nil {
			return nil, errors.New("metadata must be an object")
		}
		if metadata == nil {
			metadata = map[string]json.RawMessage{}
		}
	}
	if channel == "vibe" {
		injectVibeMetadata(metadata)
	} else {
		delete(metadata, "call_source")
	}
	if len(metadata) == 0 {
		delete(root, "metadata")
	} else {
		root["metadata"], _ = json.Marshal(metadata)
	}
	return json.Marshal(root)
}

// injectVibeMetadata stamps the full request attribution the official Mistral
// Vibe CLI sends (TelemetryRequestMetadata), so the upstream meters the call
// against the Vibe allowance exactly like a real Vibe session. Client-supplied
// metadata keys win; only the quota marker itself is always overwritten.
func injectVibeMetadata(metadata map[string]json.RawMessage) {
	fill := func(key, value string) {
		if _, exists := metadata[key]; !exists {
			b, _ := json.Marshal(value)
			metadata[key] = b
		}
	}
	metadata["call_source"] = json.RawMessage(`"vibe_code"`)
	fill("host_kind", "local")
	fill("call_type", "main_call")
	fill("version", vibeClientVersion)
	fill("agent_version", vibeClientVersion)
	fill("agent_entrypoint", "cli")
	fill("client_name", "vibe-cli")
	fill("client_version", vibeClientVersion)
	fill("session_id", vibeSessionID)
	fill("terminal_emulator", "unknown")
	fill("message_id", newVibeMessageID())
	// Official client reports the account's product plan (Pro) in metadata.
	fill("user_plan", "Pro")
	fill("os", runtime.GOOS)
	fill("arch", runtime.GOARCH)
	if vibeOSVersion != "" {
		fill("os_version", vibeOSVersion)
	}
}

// newVibeMessageID mirrors the official Vibe turn id format
// ("user-turn-<32 hex>-<turn index>"). Each proxied call is its own turn,
// so the suffix is always 1.
func newVibeMessageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "user-turn-" + randomID() + "-1"
	}
	return fmt.Sprintf("user-turn-%x-1", b)
}

// vibeRequestHeaders returns the extra upstream headers official Vibe sends on
// its mistral backend: the SDK-style User-Agent and the x-affinity cache route.
// Nil for non-vibe channels so studio/auto traffic keeps its plain shape.
func vibeRequestHeaders(key apiKey) map[string]string {
	if activeChannel(key) != "vibe" {
		return nil
	}
	return map[string]string{
		"User-Agent": vibeUserAgent,
		"X-Affinity": vibeSessionID,
	}
}

// officialMaxRequestImages mirrors Vibe's _MAX_REQUEST_IMAGES client-side cap.
const officialMaxRequestImages = 8

// imageLimitFor resolves the per-request image cap: model override wins, then the
// plugin-level default, then official parity. Negative disables trimming.
func imageLimitFor(settings pluginState, model modelMapping) int {
	if model.ImageLimit != 0 {
		return model.ImageLimit
	}
	if settings.ImageLimit != 0 {
		return settings.ImageLimit
	}
	return officialMaxRequestImages
}

func transformRequest(raw []byte, model, thinkingMap string, imageLimit int) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	modelJSON, _ := json.Marshal(model)
	root["model"] = modelJSON
	if effort, ok := rawString(root["reasoning_effort"]); ok {
		if mapped := mapThinking(effort, thinkingMap); mapped != "" {
			root["reasoning_effort"], _ = json.Marshal(mapped)
		} else {
			delete(root, "reasoning_effort")
		}
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(root["messages"], &messages); err != nil {
		return nil, errors.New("messages must be an array")
	}
	for _, message := range messages {
		// Client cache hints are not part of Mistral's content chunk schema.
		var contentParts []map[string]json.RawMessage
		if json.Unmarshal(message["content"], &contentParts) == nil {
			changed := false
			for _, part := range contentParts {
				if _, exists := part["cache_control"]; exists {
					delete(part, "cache_control")
					changed = true
				}
			}
			if changed {
				message["content"], _ = json.Marshal(contentParts)
			}
		}
		var role string
		_ = json.Unmarshal(message["role"], &role)
		// Mistral only accepts system/user/assistant/tool. OpenAI Responses-style
		// "developer" roles arrive from upstream converters and 422 upstream, so
		// fold them into system here. Official Vibe never sends developer.
		if role == "developer" {
			message["role"], _ = json.Marshal("system")
		}
		var reasoning strings.Builder
		for _, field := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
			if rawReasoning, exists := message[field]; exists {
				reasoning.WriteString(extractReasoning(rawReasoning))
				delete(message, field)
			}
		}
		if role != "assistant" || strings.TrimSpace(reasoning.String()) == "" {
			continue
		}
		parts := make([]json.RawMessage, 0, 2)
		thinkingPart, _ := json.Marshal(map[string]any{"type": "thinking", "thinking": []map[string]string{{"type": "text", "text": reasoning.String()}}})
		parts = append(parts, thinkingPart)
		content := bytes.TrimSpace(message["content"])
		if len(content) > 0 && content[0] == '[' {
			var existing []json.RawMessage
			if json.Unmarshal(content, &existing) == nil {
				parts = append(parts, existing...)
			}
		} else {
			var contentText string
			if json.Unmarshal(content, &contentText) == nil && contentText != "" {
				textPart, _ := json.Marshal(map[string]string{"type": "text", "text": contentText})
				parts = append(parts, textPart)
			}
		}
		message["content"], _ = json.Marshal(parts)
	}
	if imageLimit >= 0 {
		trimRequestImages(messages, imageLimit)
	}
	root["messages"], _ = json.Marshal(messages)
	return json.Marshal(root)
}

// trimRequestImages replicates Vibe's _keep_recent_images: walking messages from
// newest to oldest, every image_url part beyond the limit is replaced in place
// with a text placeholder so the request stays within the upstream cap.
func trimRequestImages(messages []map[string]json.RawMessage, limit int) {
	placeholder, _ := json.Marshal(map[string]string{
		"type": "text",
		"text": fmt.Sprintf("[Image removed: only the %d most recent images are sent]", limit),
	})
	kept := 0
	for i := len(messages) - 1; i >= 0; i-- {
		var contentParts []json.RawMessage
		if json.Unmarshal(messages[i]["content"], &contentParts) != nil {
			continue
		}
		changed := false
		for j := len(contentParts) - 1; j >= 0; j-- {
			var part map[string]json.RawMessage
			if json.Unmarshal(contentParts[j], &part) != nil {
				continue
			}
			var partType string
			if json.Unmarshal(part["type"], &partType) != nil || partType != "image_url" {
				continue
			}
			if kept < limit {
				kept++
				continue
			}
			contentParts[j] = placeholder
			changed = true
		}
		if changed {
			messages[i]["content"], _ = json.Marshal(contentParts)
		}
	}
}

func extractReasoning(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var items []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) == nil {
		var result strings.Builder
		for _, item := range items {
			result.WriteString(item.Text)
		}
		return result.String()
	}
	var detail struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &detail)
	return detail.Text
}

func mapThinking(value, mapping string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if mapping == "" {
		if value == "off" {
			return "none"
		}
		return value
	}
	for _, pair := range strings.Split(mapping, ",") {
		from, to, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.EqualFold(strings.TrimSpace(from), value) {
			return strings.ToLower(strings.TrimSpace(to))
		}
	}
	return value
}

func ensureStreamUsage(body []byte) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	var options map[string]json.RawMessage
	if len(root["stream_options"]) > 0 {
		_ = json.Unmarshal(root["stream_options"], &options)
	}
	if options == nil {
		options = make(map[string]json.RawMessage)
	}
	options["include_usage"] = json.RawMessage("true")
	root["stream_options"], _ = json.Marshal(options)
	return json.Marshal(root)
}

func setJSONField(raw []byte, key string, value any) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	root[key], _ = json.Marshal(value)
	return json.Marshal(root)
}

func rawString(raw json.RawMessage) (string, bool) {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func completionURL(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = defaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("base URL must be an absolute HTTP(S) URL without credentials")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/chat/completions"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func upstreamStatusError(status int, body []byte) error {
	message := "Mistral API rejected the request"
	var parsed struct {
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		var nested struct {
			Message string          `json:"message"`
			Detail  json.RawMessage `json:"detail"`
		}
		_ = json.Unmarshal(parsed.Error, &nested)
		var errorText string
		_ = json.Unmarshal(parsed.Error, &errorText)
		for _, text := range []string{parsed.Message, upstreamErrorDetail(parsed.Detail), nested.Message, upstreamErrorDetail(nested.Detail), errorText} {
			if strings.TrimSpace(text) != "" {
				message = text
				break
			}
		}
	}
	if message == "Mistral API rejected the request" {
		if text := strings.TrimSpace(string(body)); text != "" && len(text) <= 600 {
			message = text
		}
	}
	if status < 400 || status > 599 {
		status = 502
	}
	return &statusError{Status: status, Message: message}
}

func upstreamErrorDetail(raw json.RawMessage) string {
	if detail, ok := rawString(raw); ok {
		return detail
	}
	var details []struct {
		Location []json.RawMessage `json:"loc"`
		Message  string            `json:"msg"`
	}
	if json.Unmarshal(raw, &details) != nil {
		return ""
	}
	var summaries []string
	for _, detail := range details {
		if strings.TrimSpace(detail.Message) == "" {
			continue
		}
		var location strings.Builder
		for _, segment := range detail.Location {
			if field, ok := rawString(segment); ok {
				// Pydantic includes long union type descriptions in validation paths.
				if strings.ContainsAny(field, "[]") {
					continue
				}
				if location.Len() > 0 {
					location.WriteByte('.')
				}
				location.WriteString(field)
			} else {
				var index int
				if json.Unmarshal(segment, &index) == nil {
					location.WriteByte('[')
					location.WriteString(strconv.Itoa(index))
					location.WriteByte(']')
				}
			}
		}
		message := detail.Message
		if location.Len() > 0 {
			message = location.String() + ": " + message
		}
		summaries = append(summaries, message)
		if len(summaries) == 3 {
			break
		}
	}
	return strings.Join(summaries, "; ")
}

func parseUsage(raw []byte) map[string]int {
	var root struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(raw, &root) != nil || root.Usage == nil {
		return nil
	}
	usage := make(map[string]int)
	for _, name := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		var value int
		if json.Unmarshal(root.Usage[name], &value) == nil {
			usage[name] = max(0, value)
		}
	}
	var details struct {
		CachedTokens *int `json:"cached_tokens"`
	}
	_ = json.Unmarshal(root.Usage["prompt_tokens_details"], &details)
	var legacy *int
	_ = json.Unmarshal(root.Usage["num_cached_tokens"], &legacy)
	if details.CachedTokens != nil {
		usage["cached_tokens"] = max(0, *details.CachedTokens)
	}
	if legacy != nil {
		usage["cached_tokens"] = max(usage["cached_tokens"], *legacy)
	}
	return usage
}

func parseSSEUsage(line []byte) map[string]int {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil
	}
	data := bytes.TrimSpace(trimmed[len("data:"):])
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	return parseUsage(data)
}
