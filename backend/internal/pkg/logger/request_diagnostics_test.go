package logger

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type diagnosticCapabilitySink struct{ enabled bool }

func requireDiagnosticType[T any](t *testing.T, value any) T {
	t.Helper()
	typed, ok := value.(T)
	if !ok {
		t.Fatalf("unexpected diagnostic value type: %T", value)
	}
	return typed
}

func (*diagnosticCapabilitySink) WriteLogEvent(*LogEvent)           {}
func (s *diagnosticCapabilitySink) RequestDiagnosticsEnabled() bool { return s.enabled }

func TestRequestDiagnosticsEnabled_OptionalCapability(t *testing.T) {
	previous := loadSink()
	t.Cleanup(func() { SetSink(previous) })
	SetSink(nil)
	if RequestDiagnosticsEnabled() {
		t.Fatal("missing sink must disable diagnostics")
	}
	SetSink(struct{ Sink }{})
	if RequestDiagnosticsEnabled() {
		t.Fatal("sink without optional capability must disable diagnostics")
	}
	SetSink(&diagnosticCapabilitySink{})
	if RequestDiagnosticsEnabled() {
		t.Fatal("disabled sink must disable diagnostics")
	}
	SetSink(&diagnosticCapabilitySink{enabled: true})
	if !RequestDiagnosticsEnabled() {
		t.Fatal("enabled capability not detected")
	}
}

func TestSafeRequestDiagnosticContextFields(t *testing.T) {
	core := newSinkCore().Wrap(zapcore.NewNopCore())
	log := zap.New(core).With(
		zap.String("request_id", "req-123"),
		zap.Int64("user_id", 7),
		zap.String("model", "gpt-5.4"),
		zap.String("authorization", "Bearer secret"),
		zap.String("client_request_id", "sk-secret"),
	).With(zap.String("platform", "openai"))
	fields := SafeRequestDiagnosticContextFields(IntoContext(context.Background(), log))
	if len(fields) != 4 || fields["request_id"] != "req-123" || fields["user_id"] != int64(7) || fields["platform"] != "openai" {
		t.Fatalf("unexpected safe metadata: %#v", fields)
	}
	if got := SafeRequestDiagnosticContextFields(IntoContext(context.Background(), zap.NewNop())); got == nil || len(got) != 0 {
		t.Fatalf("unsupported logger should return empty nonnil map: %#v", got)
	}
}

func TestProjectRequestDiagnosticFields_RejectsUnsafeAndUnboundedValues(t *testing.T) {
	fields := ProjectRequestDiagnosticFields(map[string]any{
		"request_id": "req-1", "client_request_id": "Bearer secret", "account_id": 12,
		"user_id": math.Inf(1), "api_key_id": -1, "model": strings.Repeat("m", 129),
		"platform": "https://private.example", "outcome": "failed: token=secret",
		"ingress": "messages_http", "transport": "websocket", "conn_reused": true,
		"force_new_conn": "secret", "prep_ms": 4, "acquire_ms": -1,
		"write_ms": 86400001, "first_text_ms": 0.25, "queue_wait_ms": json.Number("12"),
		"authorization": "Bearer secret", "body": map[string]any{"secret": true},
		"error": "upstream secret", "unknown_ms": 20,
	})
	for _, key := range []string{"client_request_id", "user_id", "api_key_id", "model", "platform", "outcome", "force_new_conn", "acquire_ms", "write_ms", "first_text_ms", "authorization", "body", "error", "unknown_ms"} {
		if _, ok := fields[key]; ok {
			t.Fatalf("unsafe field %q survived: %#v", key, fields)
		}
	}
	if fields["prep_ms"] != int64(4) || fields["queue_wait_ms"] != int64(12) || fields["account_id"] != int64(12) || fields["event"] != MessagesWSLatencyEvent {
		t.Fatalf("safe values missing: %#v", fields)
	}
}

func TestProjectRequestDiagnosticSnapshots_AllSectionsRoundTrip(t *testing.T) {
	// JSON-decoded types are the storage-boundary representation.
	var input map[string]any
	if err := json.Unmarshal([]byte(`{
		"client_request":{"model":"claude-opus-4-6","stream":false,"max_tokens":0,"speed":"fast","temperature":0.5,"top_p":1,"thinking":{"type":"adaptive","budget_tokens":0},"output_config":{"effort":"high"},"tool_choice":"tool","messages_count":0,"tools_count":2},
		"upstream_request":{"type":"response.create","model":"gpt-5.4","stream":true,"store":false,"service_tier":"priority","reasoning":{"effort":"xhigh","summary":"auto"},"max_output_tokens":1000,"temperature":0,"top_p":0.75,"parallel_tool_calls":false,"text":{"verbosity":"low","format":{"type":"json_schema"}},"tool_choice":"required","input_count":0,"tools_count":2,"has_previous_response_id":false,"has_prompt_cache_key":true,"write_attempted":true,"write_completed":false},
		"client_request_headers":{"content_type":"application/json","accept":"text/event-stream","anthropic_version":"2023-06-01"},
		"upstream_request_headers":{"openai_beta":["responses_websockets=2026-02-04","responses_websockets=2026-02-06"],"connection":"upgrade","upgrade":"websocket"},
		"upstream_handshake_headers":{"connection":"keep-alive","content_type":"text/event-stream"},
		"response":{"model":"gpt-5.4","service_tier":"default","status":"incomplete","terminal_event":"response.incomplete","incomplete_reason":"max_output_tokens","error_code":"context_length_exceeded","error_type":"invalid_request_error"},
		"usage":{"input_tokens":0,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":1,"reasoning_tokens":2,"total_tokens":11}
	}`), &input); err != nil {
		t.Fatal(err)
	}
	got := ProjectRequestDiagnosticSnapshots(input)
	if len(got) != 7 {
		t.Fatalf("expected seven optional sections, got %#v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, input) {
		t.Fatalf("safe values changed: got %s", encoded)
	}
	if again := ProjectRequestDiagnosticSnapshots(got); !reflect.DeepEqual(got, again) {
		t.Fatalf("projection not idempotent: %#v", again)
	}
	stored := ProjectRequestDiagnosticFields(input)
	for key, value := range got {
		if !reflect.DeepEqual(stored[key], value) {
			t.Fatalf("storage failed to revalidate section %q", key)
		}
	}
}

func TestProjectRequestDiagnosticSnapshots_UnknownNestedCyclesAndDetached(t *testing.T) {
	const secret = "SENTINEL_PRIVATE_BODY_AUTH_AND_TOOL_NAME"
	cycle := map[string]any{}
	cycle["self"] = cycle
	cycle["model"] = cycle
	betas := []string{"responses_websockets=2026-02-06"}
	input := map[string]any{
		"client_request": map[string]any{
			"model": "gpt-5.4", "messages": cycle, "system": secret,
			"tools": []any{secret}, "metadata": cycle,
			"thinking":      map[string]any{"type": "enabled", "budget_tokens": 0, "content": secret, "secret": cycle},
			"output_config": map[string]any{"effort": "high", "schema": cycle},
			"tool_choice":   map[string]any{"type": "tool", "name": secret},
		},
		"upstream_request": map[string]any{
			"text":  map[string]any{"format": map[string]any{"type": "json_schema", "schema": cycle, "name": secret}},
			"input": cycle, "instructions": secret, "previous_response_id": secret, "prompt_cache_key": secret,
		},
		"upstream_request_headers": map[string]any{"openai_beta": betas, "authorization": secret, "cookie": secret},
		"response":                 map[string]any{"status": "failed", "error": map[string]any{"message": secret}, "delta": cycle, "body": secret},
		"usage":                    map[string]any{"input_tokens": 0, "metadata": cycle},
		"upstream_response":        cycle, "body": cycle, "error": secret,
	}
	got := ProjectRequestDiagnosticSnapshots(input)
	encoded, err := json.Marshal(got)
	if err != nil || strings.Contains(string(encoded), secret) {
		t.Fatalf("unsafe values survived: encoded=%s err=%v", encoded, err)
	}
	want := map[string]any{
		"client_request": map[string]any{
			"model": "gpt-5.4", "thinking": map[string]any{"type": "enabled", "budget_tokens": int64(0)},
			"output_config": map[string]any{"effort": "high"},
		},
		"upstream_request":         map[string]any{"text": map[string]any{"format": map[string]any{"type": "json_schema"}}},
		"upstream_request_headers": map[string]any{"openai_beta": []string{"responses_websockets=2026-02-06"}},
		"response":                 map[string]any{"status": "failed"},
		"usage":                    map[string]any{"input_tokens": int64(0)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected safe projection: %#v", got)
	}
	clientRequest := requireDiagnosticType[map[string]any](t, got["client_request"])
	requireDiagnosticType[map[string]any](t, clientRequest["thinking"])["type"] = "disabled"
	upstreamHeaders := requireDiagnosticType[map[string]any](t, got["upstream_request_headers"])
	requireDiagnosticType[[]string](t, upstreamHeaders["openai_beta"])[0] = "changed"
	inputRequest := requireDiagnosticType[map[string]any](t, input["client_request"])
	if requireDiagnosticType[map[string]any](t, inputRequest["thinking"])["type"] != "enabled" || betas[0] != "responses_websockets=2026-02-06" {
		t.Fatal("projection aliases input maps or slices")
	}
}

func TestProjectRequestDiagnosticSnapshots_MalformedOptionalSections(t *testing.T) {
	for _, input := range []map[string]any{
		nil,
		{},
		{"client_request": nil, "response": []any{"secret"}, "usage": "secret"},
		{"client_request": map[string]any{}, "upstream_request": map[string]any{"thinking": map[string]any{"type": "enabled"}}},
		{"client_request": map[string]any{"stream": "false", "max_tokens": "0", "thinking": []any{}, "output_config": nil, "speed": "fast secret"}},
		{"upstream_request": map[string]any{"reasoning": []any{}, "text": map[string]any{"format": "json_schema"}, "store": 0}},
	} {
		if got := ProjectRequestDiagnosticSnapshots(input); got == nil || len(got) != 0 {
			t.Fatalf("malformed/missing sections must be absent: %#v", got)
		}
	}
	got := ProjectRequestDiagnosticSnapshots(map[string]any{
		"client_request":   map[string]any{"stream": false, "max_tokens": 0},
		"upstream_request": map[string]any{"store": false, "write_attempted": false, "write_completed": false},
		"usage":            map[string]any{"input_tokens": 0},
	})
	if len(got) != 3 || len(requireDiagnosticType[map[string]any](t, got["client_request"])) != 2 || len(requireDiagnosticType[map[string]any](t, got["upstream_request"])) != 3 || len(requireDiagnosticType[map[string]any](t, got["usage"])) != 1 {
		t.Fatalf("zero/false lost or absent fields defaulted: %#v", got)
	}
}

func TestProjectRequestDiagnosticSnapshots_NumericBoundsAndTypes(t *testing.T) {
	valid := []any{int(0), int8(1), int16(2), int32(3), int64(4), uint(5), uint8(6), uint16(7), uint32(8), uint64(9), float32(10), float64(11), json.Number("12"), json.Number("1e3"), json.Number("1.0"), int64(1_000_000_000)}
	for _, value := range valid {
		got := ProjectRequestDiagnosticSnapshots(map[string]any{"usage": map[string]any{"input_tokens": value}})
		if len(got) != 1 {
			t.Fatalf("valid count %T(%v) omitted", value, value)
		}
	}
	invalid := []any{nil, true, "0", -1, int64(-1), uint64(math.MaxUint64), 1_000_000_001, 0.5, math.NaN(), math.Inf(1), math.Inf(-1), json.Number("NaN"), json.Number("1e999"), json.Number("1e-999"), json.Number("01"), json.Number("1.00000000000000001"), json.Number("0.9999999999999999999"), json.Number("-1e-999"), json.Number(strings.Repeat("1", 10000)), []any{0}}
	for _, value := range invalid {
		got := ProjectRequestDiagnosticSnapshots(map[string]any{"usage": map[string]any{"input_tokens": value}})
		if len(got) != 0 {
			t.Fatalf("invalid count %T(%v) survived: %#v", value, value, got)
		}
	}
	for _, value := range []any{-0.1, 2.01, math.Inf(1), "0.5", json.Number("2.000000000000000001")} {
		if got := ProjectRequestDiagnosticSnapshots(map[string]any{"client_request": map[string]any{"temperature": value}}); len(got) != 0 {
			t.Fatalf("invalid temperature survived: %#v", got)
		}
	}
	for _, value := range []any{-0.1, 1.01, math.NaN(), "0", json.Number("1.000000000000000001")} {
		if got := ProjectRequestDiagnosticSnapshots(map[string]any{"client_request": map[string]any{"top_p": value}}); len(got) != 0 {
			t.Fatalf("invalid top_p survived: %#v", got)
		}
	}
}

func TestProjectRequestDiagnosticSnapshots_RejectsMaliciousModelsHeadersAndEnums(t *testing.T) {
	for _, model := range []string{"", strings.Repeat("m", 129), "Bearer secret", "sk-private", "gpt/sk-private", "gpt-secret-value", "gpt-authorization", "gpt/session-private", "https://private", "gpt-5.4\nsecret", "gpt-5.4?token=private", "gpt-中文"} {
		got := ProjectRequestDiagnosticSnapshots(map[string]any{"client_request": map[string]any{"model": model}, "response": map[string]any{"model": model}})
		if len(got) != 0 {
			t.Fatalf("unsafe model %q survived: %#v", model, got)
		}
	}
	for _, headers := range []map[string]any{
		{"Authorization": "Bearer secret", "authorization": "Bearer secret", "cookie": "secret", "set_cookie": "secret", "user_agent": "secret", "session_id": "secret", "ticket": "secret", "anthropic_beta": "secret"},
		{"content_type": "application/json; secret=private", "accept": "application/json,secret", "anthropic_version": "2023-06-01\r\nAuthorization: secret", "connection": "upgrade, secret", "upgrade": "websocket secret"},
		{"openai_beta": "responses_websockets=2026-02-06"},
		{"openai_beta": []string{"responses_websockets=2026-02-06", "private-ticket"}},
		{"openai_beta": []any{"responses_websockets=2026-02-06", map[string]any{"secret": true}}},
		{"openai_beta": []string{"responses_websockets=2026-02-06", "responses_websockets=2026-02-06", "responses_websockets=2026-02-06", "responses_websockets=2026-02-06", "responses_websockets=2026-02-06"}},
		{"openai_beta": []any{}},
	} {
		for _, section := range []string{"client_request_headers", "upstream_request_headers", "upstream_handshake_headers"} {
			if got := ProjectRequestDiagnosticSnapshots(map[string]any{section: headers}); len(got) != 0 {
				t.Fatalf("unsafe headers survived: %#v", got)
			}
		}
	}
	got := ProjectRequestDiagnosticSnapshots(map[string]any{
		"client_request":   map[string]any{"speed": "secret", "thinking": map[string]any{"type": "secret"}, "tool_choice": "named-secret"},
		"upstream_request": map[string]any{"type": "secret", "service_tier": "secret", "reasoning": map[string]any{"effort": "secret", "summary": "secret"}, "text": map[string]any{"verbosity": "secret", "format": map[string]any{"type": "secret"}}, "tool_choice": "secret"},
		"response":         map[string]any{"status": "secret", "terminal_event": "secret", "error_type": "secret", "error_code": "secret", "incomplete_reason": "secret"},
	})
	if len(got) != 0 {
		t.Fatalf("unknown enums survived: %#v", got)
	}
}

func TestProjectRequestDiagnosticSnapshots_MaximalSchemaBounded(t *testing.T) {
	// Populate every known slot at its largest supported encoding. Unknown raw
	// payload size/cycles must not affect projection work or output bounds.
	var maximal func([]diagnosticSnapshotField) map[string]any
	maximal = func(schema []diagnosticSnapshotField) map[string]any {
		out := map[string]any{}
		for _, field := range schema {
			if len(field.children) > 0 {
				out[field.key] = maximal(field.children)
				continue
			}
			switch field.kind {
			case "model":
				out[field.key] = strings.Repeat("m", 128)
			case "bool":
				out[field.key] = false
			case "count":
				out[field.key] = diagnosticSnapshotMaxCount
			case "temperature", "probability":
				out[field.key] = 0.00000000000000000001
			case "enum":
				longest := ""
				for _, value := range field.allowed {
					if len(value) > len(longest) {
						longest = value
					}
				}
				out[field.key] = longest
			case "betas":
				out[field.key] = []string{diagnosticSnapshotWSBetas[0], diagnosticSnapshotWSBetas[1], diagnosticSnapshotWSBetas[0], diagnosticSnapshotWSBetas[1]}
			}
		}
		return out
	}
	input := maximal(diagnosticSnapshotSchema)
	input["body"] = strings.Repeat("PRIVATE", 1<<18)
	input["metadata"] = input
	got := ProjectRequestDiagnosticSnapshots(input)
	if len(got) != 7 {
		t.Fatalf("valid maximal sections dropped: %#v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil || len(encoded) > diagnosticSnapshotMaxBytes {
		t.Fatalf("snapshot bytes exceed cap: %d err=%v", len(encoded), err)
	}
	var measure func(any, int) (int, int)
	measure = func(value any, depth int) (int, int) {
		nodes, maxDepth := 1, depth
		switch v := value.(type) {
		case map[string]any:
			for _, child := range v {
				childNodes, childDepth := measure(child, depth+1)
				nodes += childNodes
				if childDepth > maxDepth {
					maxDepth = childDepth
				}
			}
		case []string:
			nodes += len(v)
			maxDepth = depth + 1
		}
		return nodes, maxDepth
	}
	nodes, depth := measure(got, 1)
	if nodes > diagnosticSnapshotMaxNodes || depth > diagnosticSnapshotMaxDepth {
		t.Fatalf("snapshot structure exceeds cap: nodes=%d depth=%d", nodes, depth)
	}
}
