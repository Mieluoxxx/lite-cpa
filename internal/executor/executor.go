package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Mieluoxxx/lite-cpa/internal/httpx"
	"github.com/Mieluoxxx/lite-cpa/internal/registry"
	"github.com/Mieluoxxx/lite-cpa/internal/thinking"
	"github.com/Mieluoxxx/lite-cpa/internal/translator"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const streamScanMax = 1 << 20 // 1MiB

const anthropicFastModeBeta = "fast-mode-2026-02-01"

type Result struct {
	Status  int
	Headers http.Header
	Body    []byte
}

type StreamResult struct {
	Status  int
	Headers http.Header
	Chunks  <-chan StreamChunk
}

type StreamChunk struct {
	Payload  []byte
	Err      error
	LogError string
}

type StatusError struct {
	Code int
	Body string
}

func (e StatusError) Error() string {
	if e.Body != "" {
		return e.Body
	}
	return http.StatusText(e.Code)
}

func (e StatusError) StatusCode() int { return e.Code }

// Execute routes to the correct standard upstream protocol.
// Providers: openai | openai-response | claude (no Codex).
func Execute(ctx context.Context, key registry.UpstreamKey, upstreamModel string, source translator.Format, payload []byte, stream bool) (any, error) {
	baseModel := thinking.ParseSuffix(upstreamModel).ModelName
	from := source
	var to translator.Format
	switch key.Provider {
	case "openai":
		to = translator.FormatOpenAI
	case "openai-response":
		to = translator.FormatOpenAIResponse
	case "claude":
		to = translator.FormatClaude
	default:
		return nil, fmt.Errorf("unknown provider %q", key.Provider)
	}

	translated := translator.TranslateRequest(from, to, baseModel, payload, stream)
	translated, _ = sjson.SetBytes(translated, "model", baseModel)
	translated, err := thinking.ApplyThinking(translated, upstreamModel, from.String(), to.String(), key.Provider)
	if err != nil {
		return nil, err
	}
	key, translated = applySpeed(key, baseModel, translated)
	translated = applyModelVerbosity(to, baseModel, key.Verbosity, translated)

	switch key.Provider {
	case "openai":
		if stream {
			result, err := executeOpenAIStream(ctx, key, from, to, baseModel, payload, translated)
			return guardResponsesClientStream(ctx, from, baseModel, result, err)
		}
		return executeOpenAI(ctx, key, from, to, baseModel, payload, translated)
	case "openai-response":
		if stream {
			result, err := executeResponsesStream(ctx, key, from, to, baseModel, payload, translated)
			return guardResponsesClientStream(ctx, from, baseModel, result, err)
		}
		return executeResponses(ctx, key, from, to, baseModel, payload, translated)
	case "claude":
		if stream {
			result, err := executeClaudeStream(ctx, key, from, to, baseModel, payload, translated)
			return guardResponsesClientStream(ctx, from, baseModel, result, err)
		}
		return executeClaude(ctx, key, from, to, baseModel, payload, translated)
	default:
		return nil, fmt.Errorf("unknown provider %q", key.Provider)
	}
}

// applySpeed enforces the model-level fast tier. Configured "fast" injects the
// native tier (Anthropic speed + fast-mode beta, OpenAI Responses service_tier priority);
// without it, client-selected fast-tier fields are removed so clients cannot
// change speed or billing.
func applySpeed(key registry.UpstreamKey, model string, body []byte) (registry.UpstreamKey, []byte) {
	switch key.Provider {
	case "claude":
		body, _ = sjson.DeleteBytes(body, "speed")
		if key.Speed == "fast" {
			body, _ = sjson.SetBytes(body, "speed", "fast")
			key.Headers = appendHeaderToken(key.Headers, "Anthropic-Beta", anthropicFastModeBeta)
		}
	case "openai", "openai-response":
		body, _ = sjson.DeleteBytes(body, "service_tier")
		if key.Provider == "openai-response" && key.Speed == "fast" && isGPTModel(model) {
			body, _ = sjson.SetBytes(body, "service_tier", "priority")
		}
	}
	return key, body
}

func isGPTModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-")
}

// applyModelVerbosity injects a configured model-level verbosity hint (low|medium|high)
// as text.verbosity in Responses requests. Verbosity is honored only for Responses
// requests on GPT models: a client-set text.verbosity wins over the configured hint,
// and a non-GPT model has any client text.verbosity stripped. Ignored for non-Responses
// upstreams.
func applyModelVerbosity(to translator.Format, model, verbosity string, body []byte) []byte {
	if to != translator.FormatOpenAIResponse {
		return body
	}
	if !isGPTModel(model) {
		body, _ = sjson.DeleteBytes(body, "text.verbosity")
		return body
	}
	if verbosity == "" {
		return body
	}
	if gjson.GetBytes(body, "text.verbosity").Exists() {
		return body
	}
	body, _ = sjson.SetBytes(body, "text.verbosity", verbosity)
	return body
}

func appendHeaderToken(headers map[string]string, name, token string) map[string]string {
	merged := make(map[string]string, len(headers)+1)
	seen := make(map[string]struct{})
	values := make([]string, 0, 4)
	appendTokens := func(raw string) {
		for _, value := range strings.Split(raw, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			values = append(values, value)
		}
	}
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			appendTokens(value)
			continue
		}
		merged[key] = value
	}
	appendTokens(token)
	merged[name] = strings.Join(values, ",")
	return merged
}

func executeOpenAI(ctx context.Context, key registry.UpstreamKey, from, to translator.Format, model string, original, body []byte) (*Result, error) {
	url := strings.TrimSuffix(key.BaseURL, "/") + "/chat/completions"
	resp, err := doJSON(ctx, key, url, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	var param any
	out := translator.TranslateNonStream(ctx, to, from, model, original, body, data, &param)
	return &Result{Status: resp.StatusCode, Headers: resp.Header.Clone(), Body: out}, nil
}

func executeOpenAIStream(ctx context.Context, key registry.UpstreamKey, from, to translator.Format, model string, original, body []byte) (*StreamResult, error) {
	body, _ = sjson.SetBytes(body, "stream_options.include_usage", true)
	body, _ = sjson.SetBytes(body, "stream", true)
	url := strings.TrimSuffix(key.BaseURL, "/") + "/chat/completions"
	resp, err := doJSON(ctx, key, url, body, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	// Same-format: forward upstream SSE verbatim so [DONE] and framing stay intact.
	if from == to {
		return streamPassthrough(ctx, resp)
	}
	return streamSSE(ctx, resp, from, to, model, original, body), nil
}

func executeResponses(ctx context.Context, key registry.UpstreamKey, from, to translator.Format, model string, original, body []byte) (*Result, error) {
	// Standard OpenAI Responses API: POST {base}/responses
	body, _ = sjson.SetBytes(body, "stream", false)
	url := strings.TrimSuffix(key.BaseURL, "/") + "/responses"
	resp, err := doJSON(ctx, key, url, body, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	var param any
	out := translator.TranslateNonStream(ctx, to, from, model, original, body, data, &param)
	return &Result{Status: resp.StatusCode, Headers: resp.Header.Clone(), Body: out}, nil
}

func executeResponsesStream(ctx context.Context, key registry.UpstreamKey, from, to translator.Format, model string, original, body []byte) (*StreamResult, error) {
	body, _ = sjson.SetBytes(body, "stream", true)
	url := strings.TrimSuffix(key.BaseURL, "/") + "/responses"
	resp, err := doJSON(ctx, key, url, body, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	// Same-format: preserve event:/data: association (do not reframe each line).
	if from == to {
		return streamPassthrough(ctx, resp)
	}
	return streamSSE(ctx, resp, from, to, model, original, body), nil
}

func executeClaude(ctx context.Context, key registry.UpstreamKey, from, to translator.Format, model string, original, body []byte) (*Result, error) {
	// Cross-format non-stream clients still request Anthropic SSE because the
	// ported NonStream translators aggregate data: event lines into one JSON body.
	// Same-format clients get a normal non-stream JSON message.
	useUpstreamStream := from != to
	body, _ = sjson.SetBytes(body, "stream", useUpstreamStream)

	base := key.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	url := strings.TrimSuffix(base, "/") + "/v1/messages"
	resp, err := doClaude(ctx, key, url, body, useUpstreamStream)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	var param any
	out := translator.TranslateNonStream(ctx, to, from, model, original, body, data, &param)
	return &Result{Status: resp.StatusCode, Headers: resp.Header.Clone(), Body: out}, nil
}

func executeClaudeStream(ctx context.Context, key registry.UpstreamKey, from, to translator.Format, model string, original, body []byte) (*StreamResult, error) {
	body, _ = sjson.SetBytes(body, "stream", true)
	base := key.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	url := strings.TrimSuffix(base, "/") + "/v1/messages"
	resp, err := doClaude(ctx, key, url, body, true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	if from == to {
		return streamPassthrough(ctx, resp)
	}
	return streamSSE(ctx, resp, from, to, model, original, body), nil
}

// ExecuteImage forwards an OpenAI Images API request verbatim to an upstream
// that already speaks the standard Images API (/v1/images/generations,
// /v1/images/edits). No translation is applied: the original body — JSON or
// multipart — and its Content-Type are passed through unchanged so multipart
// edits (with image/mask files and a boundary) reach the upstream intact.
// imageEndpoint is "generations" or "edits".
func ExecuteImage(ctx context.Context, key registry.UpstreamKey, payload []byte, contentType, imageEndpoint string, stream bool) (any, error) {
	url := strings.TrimSuffix(key.BaseURL, "/") + "/images/" + imageEndpoint
	if stream {
		resp, err := doRaw(ctx, key, url, payload, contentType, true)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, StatusError{Code: resp.StatusCode, Body: string(data)}
		}
		return streamPassthrough(ctx, resp)
	}
	resp, err := doRaw(ctx, key, url, payload, contentType, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	return &Result{Status: resp.StatusCode, Headers: resp.Header.Clone(), Body: data}, nil
}

func doJSON(ctx context.Context, key registry.UpstreamKey, url string, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+key.APIKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
	}
	applyCustomHeaders(req, key.Headers)
	return httpx.Do(ctx, httpx.Client(key.ProxyURL), req)
}

// doRaw is like doJSON but preserves the caller's Content-Type. Image edits are
// uploaded as multipart/form-data, whose boundary must travel to the upstream
// unchanged; doJSON hard-codes application/json and would break those uploads.
func doRaw(ctx context.Context, key registry.UpstreamKey, url string, body []byte, contentType string, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	if key.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+key.APIKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
	}
	applyCustomHeaders(req, key.Headers)
	return httpx.Do(ctx, httpx.Client(key.ProxyURL), req)
}

func doClaude(ctx context.Context, key registry.UpstreamKey, url string, body []byte, stream bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if key.APIKey != "" {
		req.Header.Set("x-api-key", key.APIKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Accept-Encoding", "identity")
	}
	applyCustomHeaders(req, key.Headers)
	return httpx.Do(ctx, httpx.Client(key.ProxyURL), req)
}

func applyCustomHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		if strings.EqualFold(k, "x-lite-upstream-model") {
			continue
		}
		if k != "" && v != "" {
			req.Header.Set(k, v)
		}
	}
}

func streamSSE(ctx context.Context, resp *http.Response, from, to translator.Format, model string, original, translated []byte) *StreamResult {
	out := make(chan StreamChunk, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), streamScanMax)
		var param any
		sawOpenAIDONE := false
		for scanner.Scan() {
			line := scanner.Bytes()
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				continue
			}
			chunks := translator.TranslateStream(ctx, to, from, model, original, translated, bytes.Clone(trimmed), &param)
			for _, c := range chunks {
				if len(c) == 0 {
					continue
				}
				if from == translator.FormatOpenAI && isOpenAIDONE(c) {
					sawOpenAIDONE = true
				}
				framed := frameForClient(c, from)
				select {
				case out <- StreamChunk{Payload: framed}:
				case <-ctx.Done():
					return
				}
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case out <- StreamChunk{Err: err}:
			case <-ctx.Done():
			}
			return
		}
		// Flush translators that emit terminal events on [DONE].
		chunks := translator.TranslateStream(ctx, to, from, model, original, translated, []byte("data: [DONE]"), &param)
		for _, c := range chunks {
			if len(c) == 0 {
				continue
			}
			if from == translator.FormatOpenAI && isOpenAIDONE(c) {
				sawOpenAIDONE = true
			}
			framed := frameForClient(c, from)
			select {
			case out <- StreamChunk{Payload: framed}:
			case <-ctx.Done():
				return
			}
		}
		// OpenAI chat clients expect a single terminal data: [DONE] event.
		if from == translator.FormatOpenAI && !sawOpenAIDONE {
			select {
			case out <- StreamChunk{Payload: []byte("data: [DONE]\n\n")}:
			case <-ctx.Done():
			}
		}
	}()
	return &StreamResult{Status: resp.StatusCode, Headers: resp.Header.Clone(), Chunks: out}
}

func isOpenAIDONE(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[5:])
	}
	return bytes.Equal(trimmed, []byte("[DONE]"))
}

// frameForClient formats a translator chunk for the client protocol.
// OpenAI chat: each chunk is one data: event terminated by \n\n.
// Responses/Claude: complete multi-field events (event: + data:) get a single
// trailing \n\n; lone event: field lines get a single \n; data: closes the event.
func frameForClient(chunk []byte, client translator.Format) []byte {
	if len(chunk) == 0 {
		return chunk
	}
	if bytes.HasSuffix(chunk, []byte("\n\n")) {
		return chunk
	}
	trimmedRight := bytes.TrimRight(chunk, "\r\n")

	switch client {
	case translator.FormatOpenAI:
		if bytes.HasPrefix(trimmedRight, []byte("data:")) {
			out := make([]byte, 0, len(trimmedRight)+2)
			out = append(out, trimmedRight...)
			out = append(out, '\n', '\n')
			return out
		}
		out := make([]byte, 0, len(trimmedRight)+8)
		out = append(out, "data: "...)
		out = append(out, trimmedRight...)
		out = append(out, '\n', '\n')
		return out
	default:
		// Multi-line payload already containing both event: and data: (common for
		// Responses/Claude converters via SSEEventData / AppendSSEEventBytes).
		if bytes.Contains(trimmedRight, []byte("\n")) {
			out := make([]byte, 0, len(trimmedRight)+2)
			out = append(out, trimmedRight...)
			out = append(out, '\n', '\n')
			return out
		}
		// Single field lines.
		if bytes.HasPrefix(trimmedRight, []byte("event:")) ||
			bytes.HasPrefix(trimmedRight, []byte("id:")) ||
			bytes.HasPrefix(trimmedRight, []byte("retry:")) {
			out := make([]byte, 0, len(trimmedRight)+1)
			out = append(out, trimmedRight...)
			out = append(out, '\n')
			return out
		}
		if bytes.HasPrefix(trimmedRight, []byte("data:")) {
			out := make([]byte, 0, len(trimmedRight)+2)
			out = append(out, trimmedRight...)
			out = append(out, '\n', '\n')
			return out
		}
		// Bare JSON → data event
		out := make([]byte, 0, len(trimmedRight)+8)
		out = append(out, "data: "...)
		out = append(out, trimmedRight...)
		out = append(out, '\n', '\n')
		return out
	}
}

const responsesMissingTerminalError = "upstream stream closed before a terminal response event"

func guardResponsesClientStream(ctx context.Context, source translator.Format, model string, result *StreamResult, err error) (*StreamResult, error) {
	if err != nil || result == nil || source != translator.FormatOpenAIResponse {
		return result, err
	}
	return ensureResponsesTerminal(ctx, result, model), nil
}

func ensureResponsesTerminal(ctx context.Context, result *StreamResult, model string) *StreamResult {
	out := make(chan StreamChunk, 16)
	go func() {
		defer close(out)
		observer := responsesTerminalObserver{model: model}

		send := func(chunk StreamChunk) bool {
			select {
			case out <- chunk:
				return true
			default:
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for chunk := range result.Chunks {
			if len(chunk.Payload) > 0 {
				observer.observe(chunk.Payload)
			}
			if chunk.Err == nil {
				if !send(chunk) {
					return
				}
				continue
			}

			if len(chunk.Payload) > 0 || chunk.LogError != "" {
				if !send(StreamChunk{Payload: chunk.Payload, LogError: chunk.LogError}) {
					return
				}
			}
			observer.finish()
			if observer.needsFailure() && ctx.Err() == nil && !errors.Is(chunk.Err, context.Canceled) {
				send(StreamChunk{Payload: observer.failureEvent(), LogError: chunk.Err.Error()})
				return
			}
			send(StreamChunk{Err: chunk.Err})
			return
		}

		observer.finish()
		if observer.needsFailure() && ctx.Err() == nil {
			send(StreamChunk{Payload: observer.failureEvent(), LogError: responsesMissingTerminalError})
		}
	}()

	return &StreamResult{Status: result.Status, Headers: result.Headers, Chunks: out}
}

type responsesTerminalObserver struct {
	pending       []byte
	response      []byte
	model         string
	maxSequence   int64
	hasSequence   bool
	terminalSeen  bool
	explicitError bool
	parseDisabled bool
}

func (o *responsesTerminalObserver) observe(chunk []byte) {
	if len(chunk) == 0 || o.parseDisabled {
		return
	}
	normalized := bytes.ReplaceAll(chunk, []byte("\r\n"), []byte("\n"))
	if len(o.pending)+len(normalized) > streamScanMax {
		o.pending = nil
		o.parseDisabled = true
		return
	}
	o.pending = append(o.pending, normalized...)
	for {
		end := bytes.Index(o.pending, []byte("\n\n"))
		if end < 0 {
			return
		}
		o.observeEvent(o.pending[:end])
		o.pending = append(o.pending[:0], o.pending[end+2:]...)
	}
}

func (o *responsesTerminalObserver) finish() {
	if len(bytes.TrimSpace(o.pending)) > 0 && !o.parseDisabled {
		o.observeEvent(o.pending)
	}
	o.pending = nil
}

func (o *responsesTerminalObserver) observeEvent(frame []byte) {
	eventName := ""
	var data bytes.Buffer
	for _, line := range bytes.Split(frame, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			eventName = strings.TrimSpace(string(trimmed[len("event:"):]))
		case bytes.HasPrefix(trimmed, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimSpace(trimmed[len("data:"):]))
		}
	}

	payload := data.Bytes()
	eventType := gjson.GetBytes(payload, "type").String()
	if eventType == "" {
		eventType = eventName
	}
	if sequence := gjson.GetBytes(payload, "sequence_number"); sequence.Exists() {
		value := sequence.Int()
		if !o.hasSequence || value > o.maxSequence {
			o.maxSequence = value
			o.hasSequence = true
		}
	}

	if eventType == "response.created" {
		response := gjson.GetBytes(payload, "response")
		if response.IsObject() {
			o.response = append(o.response[:0], response.Raw...)
		}
	}
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed":
		o.terminalSeen = true
	case "error", "response.error":
		o.explicitError = true
	}
	if eventName == "error" || eventName == "response.error" {
		o.explicitError = true
	}
}

func (o *responsesTerminalObserver) needsFailure() bool {
	return !o.terminalSeen && !o.explicitError && !o.parseDisabled
}

func (o *responsesTerminalObserver) failureEvent() []byte {
	response := bytes.Clone(o.response)
	if !json.Valid(response) || !gjson.ParseBytes(response).IsObject() {
		response = []byte(`{}`)
	}
	if gjson.GetBytes(response, "id").String() == "" {
		response, _ = sjson.SetBytes(response, "id", "resp_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
	}
	response, _ = sjson.SetBytes(response, "object", "response")
	if !gjson.GetBytes(response, "created_at").Exists() {
		response, _ = sjson.SetBytes(response, "created_at", time.Now().Unix())
	}
	if gjson.GetBytes(response, "model").String() == "" && o.model != "" {
		response, _ = sjson.SetBytes(response, "model", o.model)
	}
	if output := gjson.GetBytes(response, "output"); !output.Exists() || !output.IsArray() {
		response, _ = sjson.SetRawBytes(response, "output", []byte(`[]`))
	}
	response, _ = sjson.SetBytes(response, "status", "failed")
	response, _ = sjson.SetRawBytes(response, "error", []byte(`{"code":"server_error","message":"Upstream stream ended before a terminal response event."}`))
	if !gjson.GetBytes(response, "incomplete_details").Exists() {
		response, _ = sjson.SetRawBytes(response, "incomplete_details", []byte(`null`))
	}
	if !gjson.GetBytes(response, "usage").Exists() {
		response, _ = sjson.SetRawBytes(response, "usage", []byte(`null`))
	}

	sequence := int64(0)
	if o.hasSequence {
		sequence = o.maxSequence + 1
	}
	payload := []byte(`{"type":"response.failed","sequence_number":0,"response":{}}`)
	payload, _ = sjson.SetBytes(payload, "sequence_number", sequence)
	payload, _ = sjson.SetRawBytes(payload, "response", response)

	framed := make([]byte, 0, len(payload)+32)
	framed = append(framed, "event: response.failed\ndata: "...)
	framed = append(framed, payload...)
	framed = append(framed, '\n', '\n')
	return framed
}

func streamPassthrough(ctx context.Context, resp *http.Response) (*StreamResult, error) {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), streamScanMax)
	first, done, err := readSSEEvent(scanner)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	if errText := semanticSSEError(first); errText != "" {
		resp.Body.Close()
		return nil, StatusError{Code: http.StatusServiceUnavailable, Body: errText}
	}

	out := make(chan StreamChunk, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		emit := func(event [][]byte) bool {
			logError := semanticSSEError(event)
			for _, line := range event {
				// Empty lines retain the SSE event delimiter.
				payload := append(bytes.Clone(line), '\n')
				chunk := StreamChunk{Payload: payload, LogError: logError}
				logError = ""
				select {
				case out <- chunk:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}

		if !emit(first) {
			return
		}
		for !done {
			event, eventDone, scanErr := readSSEEvent(scanner)
			if !emit(event) {
				return
			}
			if scanErr != nil {
				select {
				case out <- StreamChunk{Err: scanErr}:
				case <-ctx.Done():
				}
				return
			}
			done = eventDone
		}
	}()
	return &StreamResult{Status: resp.StatusCode, Headers: resp.Header.Clone(), Chunks: out}, nil
}

// readSSEEvent reads one complete SSE event while retaining each source line.
func readSSEEvent(scanner *bufio.Scanner) (event [][]byte, done bool, err error) {
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		event = append(event, line)
		if len(bytes.TrimSpace(line)) == 0 {
			return event, false, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return event, true, err
	}
	return event, true, nil
}

// semanticSSEError extracts an upstream error that arrived in an otherwise
// successful HTTP SSE response. The original event remains available to the
// client after response output has started; before then callers may fail over.
func semanticSSEError(event [][]byte) string {
	eventName := ""
	data := ""
	for _, line := range event {
		trimmed := bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			eventName = strings.TrimSpace(string(trimmed[len("event:"):]))
		case bytes.HasPrefix(trimmed, []byte("data:")):
			data = strings.TrimSpace(string(trimmed[len("data:"):]))
		}
	}
	if eventName != "error" && eventName != "response.failed" {
		parsed := gjson.Parse(data)
		kind := parsed.Get("type").String()
		if kind != "error" && kind != "response.failed" && parsed.Get("response.status").String() != "failed" {
			return ""
		}
	}
	if data != "" {
		return data
	}
	return eventName
}
