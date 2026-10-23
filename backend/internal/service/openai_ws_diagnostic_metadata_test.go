package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/apicompat"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
)

func enableOpenAIWSDiagnosticTestSink(t *testing.T) *latencyDiagnosticCaptureSink {
	t.Helper()
	sink := &latencyDiagnosticCaptureSink{enabled: true}
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil) })
	return sink
}

func requireOpenAIWSDiagnosticSection(t *testing.T, sections map[string]any, key string) map[string]any {
	t.Helper()
	section, ok := sections[key].(map[string]any)
	require.True(t, ok, "%s must be a projected object, got %#v", key, sections[key])
	return section
}

func requireOpenAIWSDiagnosticNoSecrets(t *testing.T, sections map[string]any, sentinels ...string) {
	t.Helper()
	encoded, err := json.Marshal(sections)
	require.NoError(t, err)
	for _, sentinel := range sentinels {
		require.NotContains(t, string(encoded), sentinel)
	}
}

func TestOpenAIWSDiagnosticClientSnapshotDetachedBeforeNormalization(t *testing.T) {
	enableOpenAIWSDiagnosticTestSink(t)
	temperature, topP := 0.75, 0.9
	request := &apicompat.AnthropicRequest{
		Model: "claude-opus-4-6", MaxTokens: 128, Stream: false, Speed: "fast", Temperature: &temperature, TopP: &topP,
		System:       json.RawMessage(`"client-system-secret"`),
		Messages:     []apicompat.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"client-prompt-secret"`)}},
		Tools:        []apicompat.AnthropicTool{{Name: "client-tool-secret"}},
		Thinking:     &apicompat.AnthropicThinking{Type: "enabled", BudgetTokens: 64},
		OutputConfig: &apicompat.AnthropicOutputConfig{Effort: "high"},
		Metadata:     json.RawMessage(`{"user_id":"client-metadata-secret"}`),
		ToolChoice:   json.RawMessage(`{"type":"tool","name":"client-tool-choice-secret"}`),
	}
	headers := http.Header{
		"Content-Type": {"application/json; charset=utf-8"}, "Accept": {"application/json"}, "Anthropic-Version": {"2023-06-01"},
		"Authorization": {"Bearer client-auth-secret"}, "User-Agent": {"client-ua-secret"}, "Cookie": {"client-cookie-secret"},
		"Session-Id": {"client-session-secret"}, "X-Ticket": {"client-ticket-secret"},
	}
	ctx := withOpenAIWSClientDiagnostic(context.Background(), request, headers)
	request.Model = "gpt-6.1-sol"
	request.Thinking.Type = "disabled"
	temperature = 1.5
	headers.Set("Content-Type", "text/event-stream")
	diagnostic := newOpenAIWSDiagnosticMetadata(ctx)
	require.NotNil(t, diagnostic)
	client := requireOpenAIWSDiagnosticSection(t, diagnostic.sections, "client_request")
	require.Equal(t, "claude-opus-4-6", client["model"])
	require.Equal(t, false, client["stream"])
	require.Equal(t, "tool", client["tool_choice"])
	require.EqualValues(t, 128, client["max_tokens"])
	require.Equal(t, 0.75, client["temperature"])
	require.Equal(t, "enabled", requireOpenAIWSDiagnosticSection(t, client, "thinking")["type"])
	require.Equal(t, "application/json", requireOpenAIWSDiagnosticSection(t, diagnostic.sections, "client_request_headers")["content_type"])
	requireOpenAIWSDiagnosticNoSecrets(t, diagnostic.sections,
		"client-system-secret", "client-prompt-secret", "client-tool-secret", "client-metadata-secret", "client-tool-choice-secret",
		"client-auth-secret", "client-ua-secret", "client-cookie-secret", "client-session-secret", "client-ticket-secret")
}

func TestOpenAIWSDiagnosticDisabledDoesNotCollect(t *testing.T) {
	logger.SetSink(nil)
	t.Cleanup(func() { logger.SetSink(nil) })
	ctx := context.Background()
	require.Equal(t, ctx, withOpenAIWSClientDiagnostic(ctx, &apicompat.AnthropicRequest{Model: "gpt-6.1-sol"}, nil))
	require.Nil(t, newOpenAIWSDiagnosticMetadata(ctx))
	var diagnostic *openAIWSDiagnosticMetadata
	diagnostic.observeWriteAttempt(map[string]any{"input": "not retained"})
	diagnostic.observeWriteCompleted(nil)
	diagnostic.observeEvent("response.completed", []byte(`{"usage":{"input_tokens":1}}`))
	diagnostic.observeProposedHeaders(nil)
	diagnostic.observeHandshakeHeaders(nil)
}

// A raw schema/content value must never be traversed through MarshalJSON, even
// when malformed callers place it under a normally scalar key.
type openAIWSDiagnosticForbiddenMarshal struct{}

func (openAIWSDiagnosticForbiddenMarshal) MarshalJSON() ([]byte, error) {
	panic("diagnostic collection must not marshal caller-owned values")
}

func TestOpenAIWSDiagnosticWriteSnapshotScalarOnlyDetachedAndHonest(t *testing.T) {
	enableOpenAIWSDiagnosticTestSink(t)
	diagnostic := newOpenAIWSDiagnosticMetadata(context.Background())
	reasoning := map[string]any{"effort": "high", "summary": "auto", "content": openAIWSDiagnosticForbiddenMarshal{}}
	format := map[string]any{"type": "json_schema", "name": "schema-name-secret", "schema": openAIWSDiagnosticForbiddenMarshal{}}
	payload := map[string]any{
		"type": "response.create", "model": "gpt-6.1-sol", "stream": true, "store": false, "service_tier": "priority",
		"max_output_tokens": json.Number("128"), "temperature": 0.25, "top_p": 0.8, "parallel_tool_calls": true,
		"reasoning": reasoning, "text": map[string]any{"verbosity": "low", "format": format}, "tool_choice": "required",
		"input": []any{openAIWSDiagnosticForbiddenMarshal{}}, "tools": []any{openAIWSDiagnosticForbiddenMarshal{}},
		"previous_response_id": "previous-id-secret", "prompt_cache_key": "cache-key-secret", "instructions": "instructions-secret",
		"metadata": openAIWSDiagnosticForbiddenMarshal{}, "client_metadata": openAIWSDiagnosticForbiddenMarshal{},
	}
	diagnostic.observeWriteAttempt(payload)
	payload["model"] = "mutated-model"
	reasoning["effort"] = "low"
	format["type"] = "text"
	upstream := requireOpenAIWSDiagnosticSection(t, diagnostic.sections, "upstream_request")
	require.Equal(t, "gpt-6.1-sol", upstream["model"])
	require.Equal(t, "high", requireOpenAIWSDiagnosticSection(t, upstream, "reasoning")["effort"])
	text := requireOpenAIWSDiagnosticSection(t, upstream, "text")
	require.Equal(t, map[string]any{"type": "json_schema"}, requireOpenAIWSDiagnosticSection(t, text, "format"))
	require.EqualValues(t, 128, upstream["max_output_tokens"])
	require.EqualValues(t, 1, upstream["input_count"])
	require.EqualValues(t, 1, upstream["tools_count"])
	require.Equal(t, true, upstream["has_previous_response_id"])
	require.Equal(t, true, upstream["has_prompt_cache_key"])
	require.Equal(t, true, upstream["write_attempted"])
	require.Equal(t, false, upstream["write_completed"])
	diagnostic.observeWriteCompleted(errors.New("write-failure-secret"))
	require.Equal(t, false, upstream["write_completed"])
	diagnostic.observeWriteCompleted(nil)
	require.Equal(t, true, upstream["write_completed"])
	requireOpenAIWSDiagnosticNoSecrets(t, diagnostic.sections, "schema-name-secret", "previous-id-secret", "cache-key-secret", "instructions-secret", "write-failure-secret")
}

func TestOpenAIWSDiagnosticHeaderAllowlistAndFixedProvenance(t *testing.T) {
	enableOpenAIWSDiagnosticTestSink(t)
	diagnostic := newOpenAIWSDiagnosticMetadata(context.Background())
	headers := http.Header{
		"Content-Type": {"application/json; charset=utf-8"}, "Accept": {"*/*"},
		"Openai-Beta":   {"responses_websockets=2026-02-04, unknown-beta-secret", "responses_websockets=2026-02-06"},
		"Authorization": {"Bearer header-auth-secret"}, "Cookie": {"header-cookie-secret"},
		"Set-Cookie": {"header-set-cookie-secret"}, "X-Request-Id": {"header-id-secret"}, "User-Agent": {"header-ua-secret"},
	}
	diagnostic.observeProposedHeaders(headers)
	diagnostic.observeHandshakeHeaders(http.Header{"Content-Type": {"text/event-stream"}, "Set-Cookie": {"handshake-cookie-secret"}})
	headers.Set("Accept", "application/json")
	proposed := requireOpenAIWSDiagnosticSection(t, diagnostic.sections, "upstream_request_headers")
	require.Equal(t, "*/*", proposed["accept"])
	require.Equal(t, "application/json", proposed["content_type"])
	require.Equal(t, "text/event-stream", requireOpenAIWSDiagnosticSection(t, diagnostic.sections, "upstream_handshake_headers")["content_type"])
	requireOpenAIWSDiagnosticNoSecrets(t, diagnostic.sections, "unknown-beta-secret", "header-auth-secret", "header-cookie-secret", "header-set-cookie-secret", "header-id-secret", "header-ua-secret", "handshake-cookie-secret")
}

func TestOpenAIWSDiagnosticUsageAliasesPresenceAndTerminalAuthority(t *testing.T) {
	enableOpenAIWSDiagnosticTestSink(t)
	for _, tc := range []struct {
		name, terminal string
		want           map[string]any
	}{
		{"explicit-zero", `{"type":"response.completed","response":{"usage":{"input_tokens":0,"prompt_tokens":99,"output_tokens":0,"completion_tokens":99,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"cached_tokens":99,"cache_creation_tokens":99,"completion_tokens_details":{"reasoning_tokens":0},"total_tokens":0}}}`, map[string]any{"input_tokens": int64(0), "output_tokens": int64(0), "cache_read_input_tokens": int64(0), "cache_creation_input_tokens": int64(0), "reasoning_tokens": int64(0), "total_tokens": int64(0)}},
		{"aliases", `{"type":"response.done","response":{"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4,"cache_creation_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`, map[string]any{"input_tokens": int64(10), "output_tokens": int64(3), "cache_read_input_tokens": int64(4), "cache_creation_input_tokens": int64(2), "reasoning_tokens": int64(1)}},
		{"missing-usage", `{"type":"response.completed","response":{"status":"completed"}}`, nil},
		{"empty-usage", `{"type":"response.failed","response":{"usage":{}}}`, nil},
		{"sparse-usage", `{"type":"response.incomplete","response":{"usage":{"output_tokens":0}}}`, map[string]any{"output_tokens": int64(0)}},
		{"invalid-usage", `{"type":"response.cancelled","response":{"usage":{"input_tokens":"12","output_tokens":-1,"total_tokens":1.25}}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diagnostic := newOpenAIWSDiagnosticMetadata(context.Background())
			diagnostic.observeEvent("response.created", []byte(`{"type":"response.created","response":{"usage":{"input_tokens":100,"output_tokens":9,"total_tokens":109}}}`))
			require.Contains(t, diagnostic.sections, "usage")
			eventType := gjsonDiagnosticTestType(tc.terminal)
			diagnostic.observeEvent(eventType, []byte(tc.terminal))
			if tc.want == nil {
				require.NotContains(t, diagnostic.sections, "usage")
			} else {
				require.Equal(t, tc.want, requireOpenAIWSDiagnosticSection(t, diagnostic.sections, "usage"))
			}
		})
	}
}

func gjsonDiagnosticTestType(raw string) string {
	// Parsing in tests does not alter the production raw-event collector.
	var envelope struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(raw), &envelope)
	return envelope.Type
}

func TestOpenAIWSDiagnosticAllUsageAliasLocations(t *testing.T) {
	enableOpenAIWSDiagnosticTestSink(t)
	for _, tc := range []struct {
		canonical, usage string
	}{
		{"cache_read_input_tokens", `{"cache_read_input_tokens":7}`},
		{"cache_read_input_tokens", `{"cache_read_tokens":7}`},
		{"cache_read_input_tokens", `{"cached_tokens":7}`},
		{"cache_creation_input_tokens", `{"input_tokens_details":{"cache_creation_tokens":7}}`},
		{"cache_creation_input_tokens", `{"prompt_tokens_details":{"cache_write_tokens":7}}`},
		{"cache_creation_input_tokens", `{"cache_write_tokens":7}`},
		{"cache_creation_input_tokens", `{"cache_creation_input_tokens":7}`},
		{"cache_creation_input_tokens", `{"cache_write_input_tokens":7}`},
		{"cache_creation_input_tokens", `{"cache_creation_tokens":7}`},
	} {
		t.Run(tc.usage, func(t *testing.T) {
			diagnostic := newOpenAIWSDiagnosticMetadata(context.Background())
			diagnostic.observeEvent("response.completed", []byte(`{"type":"response.completed","usage":`+tc.usage+`}`))
			require.Equal(t, map[string]any{tc.canonical: int64(7)}, diagnostic.sections["usage"])
		})
	}
}

func TestOpenAIWSDiagnosticResponseMetadataOnlyRawEnums(t *testing.T) {
	enableOpenAIWSDiagnosticTestSink(t)
	diagnostic := newOpenAIWSDiagnosticMetadata(context.Background())
	message := []byte(`{"type":"response.failed","response":{"model":"gpt-6.1-sol","service_tier":"priority","status":"failed","id":"response-id-secret","output":[{"text":"response-content-secret"}],"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"response-error-secret"},"usage":{"input_tokens":5}}}`)
	diagnostic.observeEvent("response.failed", message)
	for i := range message {
		message[i] = 'x'
	}
	response := requireOpenAIWSDiagnosticSection(t, diagnostic.sections, "response")
	require.Equal(t, "gpt-6.1-sol", response["model"])
	require.Equal(t, "priority", response["service_tier"])
	require.Equal(t, "failed", response["status"])
	require.Equal(t, "response.failed", response["terminal_event"])
	require.Equal(t, "rate_limit_exceeded", response["error_code"])
	requireOpenAIWSDiagnosticNoSecrets(t, diagnostic.sections, "response-id-secret", "response-content-secret", "response-error-secret")
	diagnostic.observeEvent("error", []byte(`{"type":"error","error":{"code":"unknown-error-secret","type":"unknown-type-secret","message":"unknown-message-secret"},"model":"`+strings.Repeat("a", 129)+`"}`))
	response = requireOpenAIWSDiagnosticSection(t, diagnostic.sections, "response")
	require.Equal(t, "error", response["terminal_event"])
	require.NotContains(t, response, "error_code")
	require.NotContains(t, response, "error_type")
	require.NotContains(t, response, "model")
	require.NotContains(t, diagnostic.sections, "usage")
}
