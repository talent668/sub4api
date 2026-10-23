package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/MACOS-DO/sub4api/internal/pkg/apicompat"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/tidwall/gjson"
)

// This context contains only the detached, projected pre-normalization snapshot,
// never an AnthropicRequest, a request body, or a caller-owned header/map.
type openAIWSClientDiagnosticContextKey struct{}

type openAIWSDiagnosticMetadata struct {
	sections map[string]any
}

func withOpenAIWSClientDiagnostic(ctx context.Context, request *apicompat.AnthropicRequest, headers http.Header) context.Context {
	if !logger.RequestDiagnosticsEnabled() || request == nil {
		return ctx
	}
	client := map[string]any{
		"model": request.Model, "stream": request.Stream, "max_tokens": request.MaxTokens,
		"speed": request.Speed, "messages_count": len(request.Messages), "tools_count": len(request.Tools),
	}
	if request.Temperature != nil {
		client["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		client["top_p"] = *request.TopP
	}
	if request.Thinking != nil {
		client["thinking"] = map[string]any{"type": request.Thinking.Type, "budget_tokens": request.Thinking.BudgetTokens}
	}
	if request.OutputConfig != nil {
		client["output_config"] = map[string]any{"effort": request.OutputConfig.Effort}
	}
	if choice := gjson.GetBytes(request.ToolChoice, "type"); choice.Type == gjson.String {
		copyOpenAIWSDiagnosticScalar(client, "tool_choice", choice.String())
	}
	sections := logger.ProjectRequestDiagnosticSnapshots(map[string]any{
		"client_request": client, "client_request_headers": openAIWSDiagnosticHeaders(headers),
	})
	return context.WithValue(ctx, openAIWSClientDiagnosticContextKey{}, sections)
}

func newOpenAIWSDiagnosticMetadata(ctx context.Context) *openAIWSDiagnosticMetadata {
	if !logger.RequestDiagnosticsEnabled() {
		return nil
	}
	sections, _ := ctx.Value(openAIWSClientDiagnosticContextKey{}).(map[string]any)
	return &openAIWSDiagnosticMetadata{sections: logger.ProjectRequestDiagnosticSnapshots(sections)}
}

// Header sections have fixed provenance: client_request_headers is inbound;
// upstream_request_headers is the proposed acquisition header set (NOT proof of
// refreshed/on-wire headers); upstream_handshake_headers is the cached handshake
// RESPONSE on the lease, which may predate this turn on a reused connection.
func openAIWSDiagnosticHeaders(headers http.Header) map[string]any {
	out := make(map[string]any, 6)
	for key, header := range map[string]string{
		"content_type": "Content-Type", "accept": "Accept", "anthropic_version": "Anthropic-Version",
		"connection": "Connection", "upgrade": "Upgrade",
	} {
		value := headers.Get(header)
		if len(value) > 128 {
			continue
		}
		value = strings.TrimSpace(value)
		if key == "content_type" {
			value, _, _ = strings.Cut(value, ";")
			value = strings.TrimSpace(value)
		}
		if key != "anthropic_version" {
			value = strings.ToLower(value)
		}
		if value != "" {
			out[key] = value
		}
	}
	// Only the two public protocol feature constants survive; never retain an
	// arbitrary beta token or any of the credential/session/ticket headers.
	beta := make([]string, 0, 2)
	for _, value := range headers.Values("OpenAI-Beta") {
		if len(value) > 256 {
			continue
		}
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "responses_websockets=2026-02-04" && token != "responses_websockets=2026-02-06" {
				continue
			}
			if len(beta) < 4 {
				beta = append(beta, token)
			}
		}
	}
	if len(beta) > 0 {
		out["openai_beta"] = beta
	}
	return out
}

func (d *openAIWSDiagnosticMetadata) setSection(name string, fields map[string]any) {
	if d == nil {
		return
	}
	projected := logger.ProjectRequestDiagnosticSnapshots(map[string]any{name: fields})
	delete(d.sections, name)
	if section, ok := projected[name]; ok {
		// GJSON string views may otherwise keep the entire raw event allocation
		// alive. Clone only the already projected, bounded safe values.
		if fields, ok := section.(map[string]any); ok {
			cloneOpenAIWSDiagnosticStrings(fields)
		}
		d.sections[name] = section
	}
}

func cloneOpenAIWSDiagnosticStrings(fields map[string]any) {
	for key, value := range fields {
		switch value := value.(type) {
		case string:
			fields[key] = strings.Clone(value)
		case map[string]any:
			cloneOpenAIWSDiagnosticStrings(value)
		}
	}
}

func (d *openAIWSDiagnosticMetadata) observeProposedHeaders(headers http.Header) {
	if d != nil {
		d.setSection("upstream_request_headers", openAIWSDiagnosticHeaders(headers))
	}
}

func (d *openAIWSDiagnosticMetadata) observeHandshakeHeaders(headers http.Header) {
	if d != nil {
		d.setSection("upstream_handshake_headers", openAIWSDiagnosticHeaders(headers))
	}
}

// Called after optional prewarm and immediately before the real WriteJSON. Only
// explicitly listed scalar fields are visited. No request map/slice is retained,
// serialized, or handed to the logger projection.
func (d *openAIWSDiagnosticMetadata) observeWriteAttempt(payload map[string]any) {
	if d == nil {
		return
	}
	request := make(map[string]any, 18)
	for _, key := range []string{
		"type", "model", "stream", "store", "service_tier", "max_output_tokens", "temperature", "top_p", "parallel_tool_calls",
	} {
		copyOpenAIWSDiagnosticScalar(request, key, payload[key])
	}
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		fields := make(map[string]any, 2)
		for _, key := range []string{"effort", "summary"} {
			copyOpenAIWSDiagnosticScalar(fields, key, reasoning[key])
		}
		request["reasoning"] = fields
	}
	if text, ok := payload["text"].(map[string]any); ok {
		fields := make(map[string]any, 2)
		copyOpenAIWSDiagnosticScalar(fields, "verbosity", text["verbosity"])
		if format, ok := text["format"].(map[string]any); ok {
			formatFields := make(map[string]any, 1)
			copyOpenAIWSDiagnosticScalar(formatFields, "type", format["type"])
			fields["format"] = formatFields
		}
		request["text"] = fields
	}
	// Named tool selections are represented only by the public mode, never by
	// a function/tool name, schema or argument. The projection validates enums.
	if choice, ok := payload["tool_choice"].(map[string]any); ok {
		copyOpenAIWSDiagnosticScalar(request, "tool_choice", choice["type"])
	} else {
		copyOpenAIWSDiagnosticScalar(request, "tool_choice", payload["tool_choice"])
	}
	if input, ok := payload["input"].([]any); ok {
		request["input_count"] = len(input)
	}
	if tools, ok := payload["tools"].([]any); ok {
		request["tools_count"] = len(tools)
	}
	request["has_previous_response_id"] = openAIWSDiagnosticHasString(payload["previous_response_id"])
	request["has_prompt_cache_key"] = openAIWSDiagnosticHasString(payload["prompt_cache_key"])
	request["write_attempted"] = true
	request["write_completed"] = false
	d.setSection("upstream_request", request)
}

func (d *openAIWSDiagnosticMetadata) observeWriteCompleted(err error) {
	if d == nil {
		return
	}
	if request, ok := d.sections["upstream_request"].(map[string]any); ok {
		request["write_completed"] = err == nil
	}
}

func openAIWSDiagnosticHasString(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) != ""
}

func copyOpenAIWSDiagnosticScalar(out map[string]any, key string, value any) {
	switch value.(type) {
	case string, bool, int, int32, int64, uint, uint32, uint64, float32, float64, json.Number:
		out[key] = value
	}
}

func (d *openAIWSDiagnosticMetadata) observeEvent(eventType string, message []byte) {
	if d == nil {
		return
	}
	// Deltas cannot update response-level metadata. Reuse the forwarder's
	// envelope/usage gates instead of parsing or projecting every text token.
	if eventType != "error" && !openAIWSEventMayContainModel(eventType) && !openAIWSMessageShouldParseUsage(eventType, message) {
		return
	}
	root := gjson.ParseBytes(message)
	response := root.Get("response")
	if !response.IsObject() {
		response = root
	}
	fields := make(map[string]any, 8)
	if previous, ok := d.sections["response"].(map[string]any); ok {
		for key, value := range previous {
			fields[key] = value
		}
	}
	for _, key := range []string{"model", "service_tier", "status"} {
		if value := response.Get(key); value.Type == gjson.String {
			fields[key] = value.String()
		}
	}
	terminal := isOpenAIWSTerminalEvent(eventType) || eventType == "error"
	if terminal {
		fields["terminal_event"] = eventType
		// A terminal snapshot is authoritative, not an additive billing update.
		// Missing fields stay missing, explicit zero stays present, and absent
		// terminal usage cannot be mistaken for the earlier partial snapshot.
		delete(d.sections, "usage")
	}
	if reason := response.Get("incomplete_details.reason"); reason.Type == gjson.String {
		fields["incomplete_reason"] = reason.String()
	}
	errorObject := response.Get("error")
	if !errorObject.IsObject() {
		errorObject = root.Get("error")
	}
	for key, path := range map[string]string{"error_code": "code", "error_type": "type"} {
		if value := errorObject.Get(path); value.Type == gjson.String {
			fields[key] = value.String()
		}
	}
	d.setSection("response", fields)
	usage := response.Get("usage")
	if !usage.IsObject() {
		usage = root.Get("usage")
	}
	if usage.IsObject() {
		d.setSection("usage", openAIWSDiagnosticUsage(usage))
	}
}

// Alias order mirrors openai_gateway_response_handling.go, but presence takes
// precedence over positivity here: these are raw observations, not billing.
func openAIWSDiagnosticUsage(usage gjson.Result) map[string]any {
	out := make(map[string]any, 6)
	for key, paths := range map[string][]string{
		"input_tokens":  {"input_tokens", "prompt_tokens"},
		"output_tokens": {"output_tokens", "completion_tokens"},
		"cache_read_input_tokens": {
			"input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens",
			"cache_read_input_tokens", "cache_read_tokens", "cached_tokens",
		},
		"cache_creation_input_tokens": {
			"input_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_write_tokens",
			"input_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_creation_tokens",
			"cache_write_tokens", "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation_tokens",
		},
		"reasoning_tokens": {"completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens"},
		"total_tokens":     {"total_tokens"},
	} {
		for _, path := range paths {
			value := usage.Get(path)
			if value.Type == gjson.Number {
				out[key] = json.Number(value.Raw)
				break
			}
		}
	}
	return out
}
