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
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
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

// isSearchModel reports whether the request targets the virtual search model.
func isSearchModel(name string) bool {
	return strings.TrimPrefix(strings.TrimSpace(name), pluginID+"/") == searchModelID
}

// completionParts holds the pieces of a final (non-streaming) completion
// that get re-emitted as synthesized SSE for search-model stream requests.
type completionParts struct {
	Reasoning string
	Content   string
	Usage     json.RawMessage
}

// parseCompletionParts extracts reasoning/content/usage from a final
// (non-streaming) chat completion body.
func parseCompletionParts(result []byte) completionParts {
	var root struct {
		Choices []struct {
			Message struct {
				ReasoningContent string          `json:"reasoning_content"`
				Content          json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	var out completionParts
	if err := json.Unmarshal(result, &root); err != nil || len(root.Choices) == 0 {
		return out
	}
	out.Reasoning = root.Choices[0].Message.ReasoningContent
	out.Usage = root.Usage
	msg := root.Choices[0].Message
	var text string
	if err := json.Unmarshal(msg.Content, &text); err == nil {
		out.Content = text
		return out
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &parts); err == nil {
		var think, plain strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "thinking":
				think.WriteString(p.Text)
			case "text":
				plain.WriteString(p.Text)
			}
		}
		if out.Reasoning == "" {
			out.Reasoning = think.String()
		}
		out.Content = plain.String()
	}
	return out
}

// emitSynthesizedCompletion streams a finished completion as SSE chunks:
// reasoning first (as reasoning_content deltas), then content, then finish +
// usage, matching the OpenAI/DeepSeek streaming shape clients expect.
func emitSynthesizedCompletion(req rpcExecutorRequest, result []byte) {
	parts := parseCompletionParts(result)
	emit := func(delta map[string]any, finish any) {
		chunk := map[string]any{
			"id": "chatcmpl-vibe-search", "object": "chat.completion.chunk", "created": 0, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(chunk)
		hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": []byte("data: " + string(b) + "\n\n")})
	}
	if parts.Reasoning != "" {
		emit(map[string]any{"role": "assistant", "reasoning_content": parts.Reasoning}, nil)
	}
	if parts.Content != "" {
		emit(map[string]any{"role": "assistant", "content": parts.Content}, nil)
	}
	emit(map[string]any{}, "stop")
	if len(parts.Usage) > 0 {
		usageChunk := map[string]any{
			"id": "chatcmpl-vibe-search", "object": "chat.completion.chunk", "created": 0, "model": req.Model,
			"choices": []any{}, "usage": parts.Usage,
		}
		b, _ := json.Marshal(usageChunk)
		hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": []byte("data: " + string(b) + "\n\n")})
	}
	hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": []byte("data: [DONE]\n\n")})
	hostCall("host.stream.close", map[string]any{"stream_id": req.StreamID})
}

func performCompletion(ctx context.Context, req rpcExecutorRequest, trace *requestTrace) ([]byte, http.Header, error) {
	settings := currentState()
	key, model, ok := resolveKeyAndModel(settings, req.Model)
	if !ok {
		return nil, nil, &statusError{Status: 404, Message: "Mistral model mapping was not found"}
	}
	trace.resolved(key, model, req.Payload)
	if dir := parseConnectorDirective(req.Payload); dir != nil && connectorModeOf(settings) != "off" {
		result, errDir := executeConnectorTool(ctx, key, req.HostCallbackID, dir.Tool, dir.Args)
		if errDir != nil {
			return nil, nil, &statusError{Status: 502, Message: "Connector execution failed: " + errDir.Error()}
		}
		trace.completeResponse()
		return synthChatCompletion(req.Model, result), http.Header{"Content-Type": []string{"application/json"}}, nil
	}
	searchActive := isSearchModel(req.Model) && connectorModeOf(settings) != "off"
	if searchActive && model.UpstreamModel == "" {
		return nil, nil, &statusError{Status: 400, Message: "vibe-search needs at least one enabled model mapping as its upstream backing"}
	}
	payload := stripConnectorFields(req.Payload)
	body, err := transformRequest(payload, model.UpstreamModel, model.ThinkingMap, imageLimitFor(settings, model))
	if err == nil {
		body, err = applyKeyChannel(body, key)
	}
	if err != nil {
		return nil, nil, &statusError{Status: 400, Message: "Invalid chat completion request"}
	}
	names, wantTools := wantsConnectorTools(req.Payload)
	if searchActive {
		wantTools = true
		if len(names) == 0 {
			names = []string{"web_search"}
		}
	}
	if wantTools && (searchActive || connectorModeOf(settings) == "loop") {
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
		if searchActive || (wantTools && connectorModeOf(settings) == "loop") {
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
	if dir := parseConnectorDirective(req.Payload); dir != nil && connectorModeOf(settings) != "off" {
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
	_, preWant := wantsConnectorTools(req.Payload)
	loopStreaming := preWant && connectorModeOf(settings) == "loop"
	if (isSearchModel(req.Model) && connectorModeOf(settings) != "off") || loopStreaming {
		// The multi-round streaming relay drops continuation rounds (host-side
		// emit is accepted but never flushed), so connector loops always run
		// non-streaming upstream and synthesize SSE output for stream clients.
		inner := req
		inner.Stream = false
		result, _, loopErr := performCompletion(ctx, inner, trace)
		go func() {
			if loopErr != nil {
				msg := loopErr.Error()
				if se, ok := loopErr.(*statusError); ok && se != nil {
					msg = se.Message
				}
				b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "server_error"}})
				hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": []byte("data: " + string(b) + "\n\ndata: [DONE]\n\n")})
				hostCall("host.stream.close", map[string]any{"stream_id": req.StreamID})
				return
			}
			emitSynthesizedCompletion(req, result)
		}()
		return http.Header{"content-type": []string{"text/event-stream"}}, nil
	}
	payload := stripConnectorFields(req.Payload)
	body, err := transformRequest(payload, model.UpstreamModel, model.ThinkingMap, imageLimitFor(settings, model))
	if err == nil {
		body, err = applyKeyChannel(body, key)
	}
	connectorNames, wantConnectorLoop := wantsConnectorTools(req.Payload)
	wantConnectorLoop = wantConnectorLoop && connectorModeOf(settings) == "loop"
	if err == nil && wantConnectorLoop {
		body, err = injectConnectorTools(body, connectorNames)
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
	var loop *streamLoopCtx
	if wantConnectorLoop {
		loop = &streamLoopCtx{ctx: ctx, key: key, settings: settings, body: body}
	}
	go relayStream(req, stream, trace, loop)
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

func dbgTrace(msg string) {
	f, err := os.OpenFile("/Users/baozipai/Documents/dsh/cpa-test/relay-debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), msg)
}

type streamLoopCtx struct {
	ctx      context.Context
	key      apiKey
	settings pluginState
	body     []byte
}

// relayStream relays upstream SSE to the downstream client. When connector
// tools are enabled (loop != nil), tool_call deltas are buffered instead of
// forwarded; if the stream ends with connector_* calls only, the calls are
// executed via the connector gateway and a fresh upstream stream is opened
// for the next round — preserving real streaming of the final answer.
func relayStream(req rpcExecutorRequest, stream upstreamStreamResponse, trace *requestTrace, loop *streamLoopCtx) {
	var streamErr error
	var usage map[string]int
	defer func() { trace.finish(usage, streamErr) }()
	defer hostCall("host.stream.close", map[string]any{"stream_id": req.StreamID})

	cur := stream
	var spent []upstreamStreamResponse
	defer func() {
		for _, s := range spent {
			s.close(req.HostCallbackID)
		}
	}()

	emitCount := 0
	emit := func(payload []byte) bool {
		emitCount++
		rawEmit, errEmit := hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "payload": payload})
		pv := string(payload)
		if len(pv) > 60 {
			pv = pv[:60]
		}
		dbgTrace(fmt.Sprintf("emit #%d ok=%v rawlen=%d payload=%s", emitCount, errEmit == nil, len(rawEmit), pv))
		if errEmit != nil {
			trace.captureResponseError(fmt.Errorf("stream emit failed: %w", errEmit))
			dbgTrace("emit failed: " + errEmit.Error())
			streamErr = &statusError{499, "客户端已断开连接"}
			if !trace.capturesDebug() {
				return false
			}
			if err := drainUpstreamStream(req, cur, trace); err != nil {
				trace.captureResponseError(err)
			}
			return false
		}
		return true
	}

	for round := 0; round < 5; round++ {
		var suppressed [][]byte
		names := map[int]string{}
		passAll := false
		var pending []byte
		fatal := false

		for {
			chunk, err := cur.read(req.HostCallbackID)
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
						if err := drainUpstreamStream(req, cur, trace); err != nil {
							trace.captureResponseError(err)
						}
					}
					return
				}
				if len(processed) == 0 {
					continue
				}
				trace.observe(processed)
				kind, idxs, nms := sseClassify(processed)
				if loop != nil && kind == "tool_call" && !passAll {
					for i, ix := range idxs {
						if nms[i] != "" {
							names[ix] = nms[i]
							if !strings.HasPrefix(nms[i], "connector_") {
								passAll = true
							}
						}
					}
					if passAll {
						for _, l := range suppressed {
							if !emit(l) {
								return
							}
						}
						suppressed = nil
						if !emit(processed) {
							return
						}
					} else {
						suppressed = append(suppressed, processed)
					}
				} else if loop != nil && kind == "finish" && len(suppressed) > 0 && !passAll {
					suppressed = append(suppressed, processed)
				} else if len(processed) > 0 {
					if !emit(processed) {
						return
					}
				}
			}
			if chunk.Done {
				trace.completeResponse()
				// flush leftover bytes
				if len(pending) > 0 {
					if lineUsage := parseSSEUsage(pending); lineUsage != nil {
						usage = lineUsage
					}
					processed, errLine := normalizeSSELine(pending)
					if errLine == nil && len(processed) > 0 {
						trace.observe(processed)
						emit(processed)
					} else if errLine != nil {
						streamErr = &statusError{502, "Mistral 流式响应中断"}
						trace.captureResponseError(errLine)
					}
				}
				break
			}
		}
		if fatal {
			return
		}
		// round end: decide loop vs finish
		dbgTrace(fmt.Sprintf("round %d end suppressed=%d passAll=%v loop=%v", round, len(suppressed), passAll, loop != nil))
		if loop == nil || passAll || len(suppressed) == 0 {
			for _, l := range suppressed {
				if !emit(l) {
					return
				}
			}
			return
		}
		// every seen tool_call must be a connector call to continue looping
		assistant, calls := assembleStreamCalls(suppressed)
		dbgTrace(fmt.Sprintf("assemble calls=%d allConn=%v names=%v", len(calls), allConnectorCalls(names), names))
		if len(calls) == 0 || !allConnectorCalls(names) {
			for _, l := range suppressed {
				if !emit(l) {
					return
				}
			}
			return
		}
		// execute connector calls, append results, open next upstream stream
		results := make([]connectorResult, 0, len(calls))
		for _, c := range calls {
			content, cerr := executeConnectorTool(loop.ctx, loop.key, req.HostCallbackID, c.Name, c.Args)
			if cerr != nil {
				content = "connector error: " + cerr.Error()
			}
			results = append(results, connectorResult{id: c.ID, content: content})
		}
		nextBody, errAppend := appendConnectorResults(loop.body, assistant, results)
		if errAppend != nil {
			streamErr = &statusError{502, "Mistral 流式响应中断"}
			return
		}
		loop.body = nextBody
		next, errNext := openConnectorStreamRound(loop, req)
		if errNext != nil {
			streamErr = &statusError{502, "Mistral 流式响应中断"}
			trace.captureResponseError(errNext)
			hostCall("host.stream.emit", map[string]any{"stream_id": req.StreamID, "error": "Mistral stream reopen failed"})
			return
		}
		spent = append(spent, cur)
		cur = next
		dbgTrace(fmt.Sprintf("round %d reopen ok", round))
		// continue to next round
	}
}

// openConnectorStreamRound opens a fresh upstream stream for a connector loop round.
func openConnectorStreamRound(loop *streamLoopCtx, req rpcExecutorRequest) (upstreamStreamResponse, error) {
	endpoint, err := completionURL(loop.key.BaseURL)
	if err != nil {
		return upstreamStreamResponse{}, err
	}
	headers := http.Header{"Authorization": []string{"Bearer " + loop.key.Secret}, "Content-Type": []string{"application/json"}, "Accept": []string{"text/event-stream"}}
	for name, value := range vibeRequestHeaders(loop.key) {
		headers.Set(name, value)
	}
	return openUpstreamHTTPStream(loop.ctx, upstreamRequest{
		Method: http.MethodPost, URL: endpoint, Headers: headers,
		Body: loop.body, HostCallbackID: req.HostCallbackID,
	}, loop.settings.ProxyURL)
}

// sseClassify inspects an SSE data line: returns "tool_call" when the delta
// carries tool_calls, "finish" when it carries finish_reason or is a usage
// frame, else "normal". Also reports per-index function names for tool calls.
func sseClassify(line []byte) (string, []int, []string) {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return "normal", nil, nil
	}
	payload := bytes.TrimSpace(t[5:])
	var root struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Index    int `json:"index"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &root); err != nil {
		return "normal", nil, nil
	}
	if len(root.Usage) > 0 && string(root.Usage) != "null" {
		return "finish", nil, nil
	}
	if len(root.Choices) == 0 {
		return "normal", nil, nil
	}
	if len(root.Choices[0].Delta.ToolCalls) > 0 {
		var idxs []int
		var nms []string
		for _, tc := range root.Choices[0].Delta.ToolCalls {
			idxs = append(idxs, tc.Index)
			nms = append(nms, tc.Function.Name)
		}
		return "tool_call", idxs, nms
	}
	if root.Choices[0].FinishReason != nil {
		return "finish", nil, nil
	}
	return "normal", nil, nil
}

func allConnectorCalls(names map[int]string) bool {
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "connector_") {
			return false
		}
	}
	return true
}

// assembleStreamCalls merges buffered tool_call delta chunks into complete
// calls and builds the replayable assistant message.
func assembleStreamCalls(lines [][]byte) (json.RawMessage, []connectorCall) {
	type acc struct {
		id, name, args string
	}
	merged := map[int]*acc{}
	var order []int
	for _, line := range lines {
		t := bytes.TrimSpace(line)
		if !bytes.HasPrefix(t, []byte("data:")) {
			continue
		}
		var root struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(t[5:]), &root); err != nil {
			continue
		}
		if len(root.Choices) == 0 {
			continue
		}
		for _, tc := range root.Choices[0].Delta.ToolCalls {
			a, ok := merged[tc.Index]
			if !ok {
				a = &acc{}
				merged[tc.Index] = a
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			a.args += tc.Function.Arguments
		}
	}
	var calls []connectorCall
	var toolCalls []map[string]any
	for _, ix := range order {
		a := merged[ix]
		if a.name == "" {
			continue
		}
		var args map[string]any
		_ = json.Unmarshal([]byte(a.args), &args)
		tool := a.name
		if i := strings.LastIndex(tool, "."); i >= 0 {
			tool = tool[i+1:]
		}
		calls = append(calls, connectorCall{ID: a.id, Name: tool, Args: args})
		toolCalls = append(toolCalls, map[string]any{
			"id": a.id, "type": "function",
			"function": map[string]any{"name": a.name, "arguments": a.args},
		})
	}
	assistant, _ := json.Marshal(map[string]any{"role": "assistant", "tool_calls": toolCalls})
	return assistant, calls
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
	// OpenAI-dialect compatibility: Mistral validates chat completion requests
	// with extra=forbid and answers 422 "extra inputs are not permitted" for
	// unknown top-level fields. Convert the ones with a Mistral equivalent and
	// strip the ones without — empirical table (2026-11, zai-glm-5-3):
	//   422: max_completion_tokens, store, seed, user, logit_bias
	//   400: top_logprobs (unsupported param)
	//   tolerated: stream_options, logprobs, penalties, n, response_format,
	//   parallel_tool_calls, tool_choice, temperature, top_p, stop
	if value, exists := root["max_completion_tokens"]; exists {
		if _, has := root["max_tokens"]; !has {
			root["max_tokens"] = value
		}
		delete(root, "max_completion_tokens")
	}
	if value, exists := root["seed"]; exists {
		if _, has := root["random_seed"]; !has {
			root["random_seed"] = value
		}
		delete(root, "seed")
	}
	for _, field := range []string{"store", "user", "logit_bias", "top_logprobs"} {
		delete(root, field)
	}
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
