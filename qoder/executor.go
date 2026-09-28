package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// basePromptRaw is Qoder's CLI request skeleton. The real chat endpoint only
// accepts this shape, so every request is built by filling it in rather than by
// translating the OpenAI body directly.
//
//go:embed baseprompt.json
var basePromptRaw string

var (
	templateOnce   sync.Once
	templateFilled []byte
)

// loadTemplate resolves the UUID/TIME placeholders once. The upstream expects
// stable ids inside the skeleton across requests.
func loadTemplate() []byte {
	templateOnce.Do(func() {
		filled := basePromptRaw
		for i := 1; i <= 5; i++ {
			filled = strings.ReplaceAll(filled, fmt.Sprintf("{UUID%d}", i), newUUID())
		}
		templateFilled = []byte(strings.ReplaceAll(filled, "{TIME1}", fmt.Sprintf("%d", time.Now().UnixMilli())))
	})
	return templateFilled
}

// qoderRequest is the subset of the OpenAI body we translate into the skeleton.
type qoderRequest struct {
	Model    string
	Messages []any
	Tools    []any
}

func decodeRequest(payload []byte) (*qoderRequest, error) {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, fmt.Errorf("request decode: %w", err)
	}
	req := &qoderRequest{}
	if m, ok := obj["model"].(string); ok {
		req.Model = m
	}
	if msgs, ok := obj["messages"].([]any); ok {
		req.Messages = msgs
	}
	if tools, ok := obj["tools"].([]any); ok && len(tools) > 0 {
		req.Tools = tools
	}
	return req, nil
}

// buildQoderBody fills the skeleton for one request. It returns the body and
// the prompt it extracted, which doubles as the upstream's session title.
func buildQoderBody(payload []byte, cat *catalog) (map[string]any, string, error) {
	req, err := decodeRequest(payload)
	if err != nil {
		return nil, "", err
	}
	var body map[string]any
	if err := json.Unmarshal(loadTemplate(), &body); err != nil {
		return nil, "", fmt.Errorf("template decode: %w", err)
	}

	prompt := latestUserPrompt(req.Messages)
	key := cat.resolveModelKey(req.Model)
	toolsEnabled := len(req.Tools) > 0

	nid := newUUID()
	body["request_id"] = nid
	body["chat_record_id"] = nid
	body["request_set_id"] = newUUID()
	body["session_id"] = newUUID()
	body["stream"] = true
	body["aliyun_user_type"] = defaultUserType

	if mc, ok := body["model_config"].(map[string]any); ok {
		mc["key"] = key
		mc["is_reasoning"] = key == "qmodel_latest"
	}

	now := time.Now().UnixMilli()
	if biz, ok := body["business"].(map[string]any); ok {
		biz["id"] = newUUID()
		biz["begin_at"] = now
		biz["name"] = truncate(prompt, 30)
	}
	if ctx, ok := body["chat_context"].(map[string]any); ok {
		if text, ok := ctx["text"].(map[string]any); ok {
			text["text"] = prompt
		}
		if extra, ok := ctx["extra"].(map[string]any); ok {
			if original, ok := extra["originalContent"].(map[string]any); ok {
				original["text"] = prompt
			}
		}
	}

	templateMessages, _ := body["messages"].([]any)
	body["messages"] = buildQoderMessages(templateMessages, req.Messages, prompt, toolsEnabled)
	if toolsEnabled {
		body["tools"] = req.Tools
	} else {
		delete(body, "tools")
	}
	return body, prompt, nil
}

// buildQoderMessages replaces the skeleton's example conversation with the
// client's. Qoder's own system prompt is only kept when the client sent tools
// and no system message of its own — otherwise the model tries to act like the
// Qoder CLI and confuses clients.
func buildQoderMessages(templateMessages, incoming []any, prompt string, toolsEnabled bool) []any {
	rebuilt := make([]any, 0, len(incoming)+1)
	if toolsEnabled && !hasRole(incoming, "system") {
		for _, m := range templateMessages {
			if msg, ok := m.(map[string]any); ok && msg["role"] == "system" {
				rebuilt = append(rebuilt, msg)
			}
		}
	}
	for i, m := range incoming {
		if converted := convertMessage(m, toolsEnabled, hasResolvedToolResponse(incoming, i)); converted != nil {
			rebuilt = append(rebuilt, converted)
		}
	}
	if len(rebuilt) == 0 && strings.TrimSpace(prompt) != "" {
		rebuilt = append(rebuilt, buildUserMessage(prompt, nil))
	}
	return rebuilt
}

func convertMessage(item any, toolsEnabled, allowToolCalls bool) any {
	msg, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	role, _ := msg["role"].(string)
	if role == "" {
		role = "user"
	}
	text := normalizeMessageText(msg)

	if role == "assistant" {
		calls, _ := msg["tool_calls"].([]any)
		if toolsEnabled && allowToolCalls && len(calls) > 0 {
			out := buildStructuredMessage(role, text)
			out["tool_calls"] = calls
			return out
		}
		if len(calls) > 0 {
			encoded, _ := json.Marshal(calls)
			return buildStructuredMessage(role, joinSections(text, "Tool calls:\n"+string(encoded)))
		}
		return buildStructuredMessage(role, text)
	}

	if role == "tool" {
		if toolsEnabled {
			out := buildStructuredMessage(role, text)
			if name, ok := msg["name"].(string); ok && name != "" {
				out["name"] = name
			}
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				out["tool_call_id"] = id
			}
			return out
		}
		return buildUserMessage(renderToolResult(msg, text), nil)
	}

	var images []any
	if role == "user" {
		images = extractImageParts(msg)
	}
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return nil
	}
	if role == "user" {
		return buildUserMessage(text, images)
	}
	return buildStructuredMessage(role, text)
}

func buildUserMessage(text string, images []any) map[string]any {
	contents := make([]any, 0, len(images)+1)
	if text != "" {
		contents = append(contents, map[string]any{"type": "text", "text": text})
	}
	contents = append(contents, images...)
	if len(contents) == 0 {
		contents = append(contents, map[string]any{"type": "text", "text": ""})
	}
	return map[string]any{
		"role":                        "user",
		"content":                     "",
		"contents":                    contents,
		"response_meta":               blankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func buildStructuredMessage(role, text string) map[string]any {
	return map[string]any{
		"role":                        role,
		"content":                     text,
		"response_meta":               blankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func blankResponseMeta() map[string]any {
	return map[string]any{
		"id": "",
		"usage": map[string]any{
			"prompt_tokens":             0,
			"completion_tokens":         0,
			"total_tokens":              0,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 0},
			"prompt_tokens_details":     map[string]any{"cached_tokens": 0},
		},
	}
}

func renderToolResult(msg map[string]any, text string) string {
	name, _ := msg["name"].(string)
	id, _ := msg["tool_call_id"].(string)
	var sb strings.Builder
	sb.WriteString("Tool result")
	if name != "" {
		sb.WriteString(" (" + name + ")")
	}
	if id != "" {
		sb.WriteString(" [" + id + "]")
	}
	if text != "" {
		sb.WriteString(":\n" + text)
	}
	return sb.String()
}

// extractImageParts pulls image_url / input_image blocks out of a message so
// they can ride along in contents, which is the only place Qoder reads them.
func extractImageParts(msg map[string]any) []any {
	content, ok := msg["content"].([]any)
	if !ok {
		return nil
	}
	var parts []any
	for _, item := range content {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if part["type"] != "image_url" && part["type"] != "input_image" {
			continue
		}
		switch iu := part["image_url"].(type) {
		case string:
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": iu}})
		case map[string]any:
			parts = append(parts, map[string]any{"type": "image_url", "image_url": iu})
		}
	}
	return parts
}

// normalizeMessageText flattens string or multimodal content into one string.
func normalizeMessageText(msg map[string]any) string {
	text := normalizeContent(msg["content"])
	if strings.TrimSpace(text) == "" {
		text = normalizeContent(msg["contents"])
	}
	return text
}

func normalizeContent(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, item := range v {
			if part := normalizeContentPart(item); part != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n\n")
				}
				sb.WriteString(part)
			}
		}
		return sb.String()
	default:
		return normalizeContentPart(v)
	}
}

func normalizeContentPart(item any) string {
	switch v := item.(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any:
		if t, ok := v["text"].(string); ok {
			return t
		}
		if v["type"] == "image_url" || v["type"] == "input_image" {
			// Images travel as structured parts, never flattened into text.
			return ""
		}
		if inner := normalizeContent(v["content"]); inner != "" {
			return inner
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(encoded)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func latestUserPrompt(messages []any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg, ok := messages[i].(map[string]any)
		if !ok || msg["role"] != "user" {
			continue
		}
		if text := strings.TrimSpace(normalizeMessageText(msg)); text != "" {
			return text
		}
	}
	return ""
}

func hasRole(messages []any, role string) bool {
	for _, m := range messages {
		if msg, ok := m.(map[string]any); ok && msg["role"] == role {
			return true
		}
	}
	return false
}

// hasResolvedToolResponse reports whether an assistant tool call at index i is
// followed by its tool result — only then can the call be sent structurally.
func hasResolvedToolResponse(messages []any, index int) bool {
	msg, ok := messages[index].(map[string]any)
	if !ok || msg["role"] != "assistant" {
		return false
	}
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) == 0 {
		return false
	}
	for i := index + 1; i < len(messages); i++ {
		next, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		switch next["role"] {
		case "tool":
			return true
		case "assistant", "user", "system":
			return false
		}
	}
	return false
}

func joinSections(first, second string) string {
	if strings.TrimSpace(first) == "" {
		return second
	}
	if strings.TrimSpace(second) == "" {
		return first
	}
	return first + "\n\n" + second
}

// -----------------------------------------------------------------------------
// Upstream SSE decoding
// -----------------------------------------------------------------------------

// sseFrame is the outer envelope. The HTTP status is always 200 — real failures
// arrive inside it as a non-200 statusCodeValue, so that must be checked.
type sseFrame struct {
	Body            any    `json:"body"`
	StatusCodeValue int    `json:"statusCodeValue"`
	StatusCode      string `json:"statusCode"`
}

// unwrapFrame digs the payload out of an upstream SSE line. done reports the
// end-of-stream frame, whose body is the literal "[DONE]" rather than JSON —
// the terminator is nested inside the envelope, not emitted as a bare
// "data: [DONE]" line, so it has to be recognised here.
func unwrapFrame(line string) (inner map[string]any, done bool, err error) {
	var frame sseFrame
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		return nil, false, fmt.Errorf("frame decode: %w", err)
	}
	if frame.StatusCodeValue != 0 && frame.StatusCodeValue != http.StatusOK {
		return nil, false, fmt.Errorf("upstream %d %s: %s", frame.StatusCodeValue, frame.StatusCode, truncate(fmt.Sprintf("%v", frame.Body), 300))
	}
	switch body := frame.Body.(type) {
	case string:
		if body == "" {
			return nil, false, nil
		}
		if body == "[DONE]" {
			return nil, true, nil
		}
		if err := json.Unmarshal([]byte(body), &inner); err != nil {
			return nil, false, fmt.Errorf("body decode: %w", err)
		}
	case map[string]any:
		inner = body
	default:
		return nil, false, nil
	}
	if code, hasCode := inner["code"]; hasCode {
		if _, hasChoices := inner["choices"]; !hasChoices {
			return nil, false, fmt.Errorf("qoder error %v: %v", code, inner["message"])
		}
	}
	return inner, false, nil
}

// chunkID and chunkModel seed the fields OpenAI clients expect on every chunk;
// the upstream omits them.
func decorateChunk(inner map[string]any, id, model string, created int64) []byte {
	if inner == nil {
		return nil
	}
	if _, ok := inner["id"]; !ok {
		inner["id"] = id
	}
	if _, ok := inner["created"]; !ok {
		inner["created"] = created
	}
	if _, ok := inner["model"]; !ok {
		inner["model"] = model
	}
	out, err := json.Marshal(inner)
	if err != nil {
		return nil
	}
	return out
}

// -----------------------------------------------------------------------------
// Executor handlers
// -----------------------------------------------------------------------------

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	cat, err := getCatalog(sa)
	if err != nil {
		return nil, err
	}
	source := req.Payload
	if len(source) == 0 {
		source = req.OriginalRequest
	}
	body, _, err := buildQoderBody(source, cat)
	if err != nil {
		return nil, err
	}
	sess, err := sa.session()
	if err != nil {
		return nil, err
	}
	// The upstream only speaks SSE, so a non-streaming client request is
	// streamed and folded back into a single chat.completion.
	resp, err := sess.openStream(endpointsFor(sa.Variant).api+pathChat, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	completion, err := aggregateCompletion(resp.Body, req.Model, cat)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: completion})
}

// executorStreamRequest wraps the host's executor.execute_stream RPC: the
// ExecutorRequest plus the async stream id the host uses to receive chunks.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	cat, err := getCatalog(sa)
	if err != nil {
		return nil, err
	}
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	qoderBody, _, err := buildQoderBody(body, cat)
	if err != nil {
		return nil, err
	}
	sess, err := sa.session()
	if err != nil {
		return nil, err
	}
	headers := streamHeaders()
	sseFramed := clientNeedsSSEFrame(req.Metadata)

	httpReqURL := endpointsFor(sa.Variant).api + pathChat
	if req.StreamID == "" {
		// No async stream id → fall back to synchronous chunk collection.
		chunks, errCollect := collectUpstreamStream(sess, httpReqURL, qoderBody, req.Model, cat, sseFramed)
		if errCollect != nil {
			return nil, errCollect
		}
		return okEnvelope(streamResponse{Headers: headers, Chunks: chunks})
	}

	// Async: return immediately with empty chunks. A goroutine pumps the upstream
	// and emits each chunk via host.stream.emit so the client sees true streaming.
	go pumpUpstreamStream(sess, httpReqURL, qoderBody, req.StreamID, req.Model, cat, sseFramed)
	return okEnvelope(streamResponse{Headers: headers})
}

func streamHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

// sseScanner reads the upstream body line by line. Chunks can be large (tool
// call arguments), so the buffer is generous.
func sseScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return scanner
}

func pumpUpstreamStream(sess *session, url string, body map[string]any, streamID, model string, cat *catalog, sseFramed bool) {
	resp, err := sess.openStream(url, body)
	if err != nil {
		streamEmitError(streamID, err.Error())
		streamClose(streamID)
		return
	}
	defer resp.Body.Close()
	id := "chatcmpl-" + strings.ReplaceAll(newUUID(), "-", "")[:24]
	created := time.Now().Unix()
	scanner := sseScanner(resp.Body)
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		inner, done, err := unwrapFrame(content)
		if err != nil {
			streamEmitError(streamID, err.Error())
			break
		}
		if done {
			break
		}
		chunk := decorateChunk(inner, id, model, created)
		if chunk == nil {
			continue
		}
		payload := string(chunk)
		if sseFramed {
			payload = "data: " + payload
		}
		if err := streamEmit(streamID, []byte(payload)); err != nil {
			break
		}
	}
	streamClose(streamID)
}

func collectUpstreamStream(sess *session, url string, body map[string]any, model string, cat *catalog, sseFramed bool) ([]pluginapi.ExecutorStreamChunk, error) {
	resp, err := sess.openStream(url, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	id := "chatcmpl-" + strings.ReplaceAll(newUUID(), "-", "")[:24]
	created := time.Now().Unix()
	var chunks []pluginapi.ExecutorStreamChunk
	scanner := sseScanner(resp.Body)
	for scanner.Scan() {
		content := stripDataPrefix(scanner.Text())
		if content == "" || content == "[DONE]" {
			continue
		}
		inner, done, err := unwrapFrame(content)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
		chunk := decorateChunk(inner, id, model, created)
		if chunk == nil {
			continue
		}
		payload := string(chunk)
		if sseFramed {
			payload = "data: " + payload
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(payload)})
	}
	return chunks, nil
}

// aggregateCompletion folds an upstream SSE stream into a single
// chat.completion object for non-streaming clients.
func aggregateCompletion(r io.Reader, model string, cat *catalog) ([]byte, error) {
	var content, reasoning, role, respModel string
	var toolCalls []map[string]any
	created := time.Now().Unix()
	id := "chatcmpl-" + strings.ReplaceAll(newUUID(), "-", "")[:24]

	scanner := sseScanner(r)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		inner, done, err := unwrapFrame(data)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
		if inner == nil {
			continue
		}
		if v, ok := inner["model"].(string); ok && v != "" {
			respModel = v
		}
		choices, _ := inner["choices"].([]any)
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				if v, ok := delta["role"].(string); ok && v != "" {
					role = v
				}
				if v, ok := delta["content"].(string); ok {
					content += v
				}
				if v, ok := delta["reasoning_content"].(string); ok {
					reasoning += v
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						if call, ok := tc.(map[string]any); ok {
							toolCalls = append(toolCalls, call)
						}
					}
				}
			}
		}
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	result := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return out, nil
}
