package logger

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"go.uber.org/zap/zapcore"
)

const (
	RequestDiagnosticsComponent = "gateway.request_diagnostics"
	MessagesWSLatencyEvent      = "openai.messages_ws_latency"
)

// RequestDiagnosticsEnabled is an optional sink capability. Diagnostic producers
// can avoid collecting fields when persistence is off; sinks still enforce admission.
func RequestDiagnosticsEnabled() bool {
	sink, ok := loadSink().(interface{ RequestDiagnosticsEnabled() bool })
	return ok && sink.RequestDiagnosticsEnabled()
}

// SafeRequestDiagnosticContextFields returns a new, nonnil map containing only
// bounded metadata from the standard contextual logger. Other cores fail closed.
// Never encode unrelated fields (e.g. zap.Any payloads or error objects).
func SafeRequestDiagnosticContextFields(ctx context.Context) map[string]any {
	fields := map[string]any{}
	if core, ok := FromContext(ctx).Core().(*sinkCore); ok {
		enc := zapcore.NewMapObjectEncoder()
		for _, f := range core.fields {
			if diagnosticMetadataKey(f.Key) {
				switch f.Type {
				case zapcore.StringType, zapcore.Int64Type, zapcore.Uint64Type:
					f.AddTo(enc)
				}
			}
		}
		fields = enc.Fields
	}
	return SafeRequestDiagnosticMetadata(fields)
}

func diagnosticMetadataKey(key string) bool {
	switch key {
	case "request_id", "client_request_id", "user_id", "api_key_id", "account_id", "model", "platform":
		return true
	}
	return false
}

// SafeRequestDiagnosticMetadata projects scalars only; invalid values are omitted,
// not truncated, to avoid retaining prefixes of bodies, headers or credentials.
func SafeRequestDiagnosticMetadata(fields map[string]any) map[string]any {
	out := make(map[string]any, 7)
	for _, key := range []string{"request_id", "client_request_id", "model", "platform"} {
		limit := 128
		if key == "platform" {
			limit = 64
		}
		if value, ok := fields[key].(string); ok && safeDiagnosticLabel(value, limit) {
			out[key] = value
		}
	}
	for _, key := range []string{"user_id", "api_key_id", "account_id"} {
		if value, ok := diagnosticInteger(fields[key]); ok && value > 0 {
			out[key] = value
		}
	}
	return out
}

func safeDiagnosticLabel(value string, limit int) bool {
	if len(value) == 0 || len(value) > limit {
		return false
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"sk-", "bearer", "eyj", "ghp_", "gho_", "xox", "http:", "https:"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	for _, ch := range value {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && !strings.ContainsRune("._:/-", ch) {
			return false
		}
	}
	return true
}

func diagnosticInteger(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case uint:
		if uint64(n) <= math.MaxInt64 {
			return int64(n), true
		}
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), true
		}
	case float64:
		if n >= 0 && n < math.MaxInt64 && math.Trunc(n) == n {
			return int64(n), true
		}
	case json.Number:
		v, err := n.Int64()
		return v, err == nil
	case string:
		if len(n) <= 19 {
			v, err := strconv.ParseInt(n, 10, 64)
			return v, err == nil
		}
	}
	return 0, false
}

const (
	diagnosticSnapshotMaxBytes = 8 * 1024
	diagnosticSnapshotMaxNodes = 128
	diagnosticSnapshotMaxDepth = 5
	diagnosticSnapshotMaxCount = 1_000_000_000
)

// A fixed schema bounds work independently of input size. Never range over input
// maps, recursively copy unknown values, invoke marshalers, or stringify values.
type diagnosticSnapshotField struct {
	key      string
	kind     string
	allowed  []string
	children []diagnosticSnapshotField
}

func diagnosticSnapshotEnum(key string, allowed ...string) diagnosticSnapshotField {
	return diagnosticSnapshotField{key: key, kind: "enum", allowed: allowed}
}

var diagnosticSnapshotEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
var diagnosticSnapshotTiers = []string{"auto", "default", "flex", "scale", "priority", "ultrafast"}
var diagnosticSnapshotWSBetas = []string{"responses_websockets=2026-02-04", "responses_websockets=2026-02-06"}

var diagnosticSnapshotHeaders = []diagnosticSnapshotField{
	diagnosticSnapshotEnum("content_type", "application/json", "text/event-stream"),
	diagnosticSnapshotEnum("accept", "application/json", "text/event-stream", "*/*"),
	diagnosticSnapshotEnum("anthropic_version", "2023-06-01"),
	{key: "openai_beta", kind: "betas", allowed: diagnosticSnapshotWSBetas},
	diagnosticSnapshotEnum("connection", "upgrade", "keep-alive", "close"),
	diagnosticSnapshotEnum("upgrade", "websocket"),
}

var diagnosticSnapshotSchema = []diagnosticSnapshotField{
	{key: "client_request", children: []diagnosticSnapshotField{
		{key: "model", kind: "model"},
		{key: "stream", kind: "bool"},
		{key: "max_tokens", kind: "count"},
		diagnosticSnapshotEnum("speed", "fast", "standard", "normal"),
		{key: "temperature", kind: "temperature"},
		{key: "top_p", kind: "probability"},
		{key: "thinking", children: []diagnosticSnapshotField{
			diagnosticSnapshotEnum("type", "enabled", "disabled", "adaptive"),
			{key: "budget_tokens", kind: "count"},
		}},
		{key: "output_config", children: []diagnosticSnapshotField{
			diagnosticSnapshotEnum("effort", diagnosticSnapshotEfforts...),
		}},
		diagnosticSnapshotEnum("tool_choice", "auto", "any", "tool", "none"),
		{key: "messages_count", kind: "count"},
		{key: "tools_count", kind: "count"},
	}},
	{key: "upstream_request", children: []diagnosticSnapshotField{
		diagnosticSnapshotEnum("type", "response.create"),
		{key: "model", kind: "model"},
		{key: "stream", kind: "bool"},
		{key: "store", kind: "bool"},
		diagnosticSnapshotEnum("service_tier", diagnosticSnapshotTiers...),
		{key: "reasoning", children: []diagnosticSnapshotField{
			diagnosticSnapshotEnum("effort", diagnosticSnapshotEfforts...),
			diagnosticSnapshotEnum("summary", "auto", "concise", "detailed"),
		}},
		{key: "max_output_tokens", kind: "count"},
		{key: "temperature", kind: "temperature"},
		{key: "top_p", kind: "probability"},
		{key: "parallel_tool_calls", kind: "bool"},
		{key: "text", children: []diagnosticSnapshotField{
			diagnosticSnapshotEnum("verbosity", "low", "medium", "high"),
			{key: "format", children: []diagnosticSnapshotField{
				diagnosticSnapshotEnum("type", "text", "json_object", "json_schema"),
			}},
		}},
		diagnosticSnapshotEnum("tool_choice", "auto", "none", "required"),
		{key: "input_count", kind: "count"},
		{key: "tools_count", kind: "count"},
		{key: "has_previous_response_id", kind: "bool"},
		{key: "has_prompt_cache_key", kind: "bool"},
		{key: "write_attempted", kind: "bool"},
		{key: "write_completed", kind: "bool"},
	}},
	{key: "client_request_headers", children: diagnosticSnapshotHeaders},
	{key: "upstream_request_headers", children: diagnosticSnapshotHeaders},
	{key: "upstream_handshake_headers", children: diagnosticSnapshotHeaders},
	{key: "response", children: []diagnosticSnapshotField{
		{key: "model", kind: "model"},
		diagnosticSnapshotEnum("service_tier", diagnosticSnapshotTiers...),
		diagnosticSnapshotEnum("status", "queued", "in_progress", "completed", "incomplete", "failed", "cancelled", "canceled"),
		diagnosticSnapshotEnum("terminal_event", "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error"),
		diagnosticSnapshotEnum("incomplete_reason", "max_output_tokens", "content_filter"),
		diagnosticSnapshotEnum("error_type", "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "rate_limit_error", "rate_limit_exceeded", "api_error", "server_error", "overloaded_error", "timeout_error", "connection_error"),
		diagnosticSnapshotEnum("error_code", "server_error", "internal_error", "rate_limit_exceeded", "insufficient_quota", "invalid_api_key", "invalid_request_error", "invalid_request", "invalid_value", "invalid_parameter", "missing_required_parameter", "unknown_parameter", "unsupported_parameter", "model_not_found", "context_length_exceeded", "max_output_tokens", "content_filter", "content_policy_violation", "previous_response_not_found", "invalid_encrypted_content", "websocket_connection_limit_reached", "websocket_session_timeout", "websocket_event_type_not_supported", "websocket_error", "request_timeout", "timeout", "cancelled", "canceled"),
	}},
	{key: "usage", children: []diagnosticSnapshotField{
		{key: "input_tokens", kind: "count"},
		{key: "output_tokens", kind: "count"},
		{key: "cache_read_input_tokens", kind: "count"},
		{key: "cache_creation_input_tokens", kind: "count"},
		{key: "reasoning_tokens", kind: "count"},
		{key: "total_tokens", kind: "count"},
	}},
}

// ProjectRequestDiagnosticSnapshots returns detached, optional structured request
// diagnostics. Producers and storage both use this whitelist; neither raw bodies
// nor arbitrary headers/error text are ever copied. Missing, empty and malformed
// sections are omitted, while explicit valid zero/false values are preserved.
func ProjectRequestDiagnosticSnapshots(fields map[string]any) map[string]any {
	budget := diagnosticSnapshotBudget{nodes: 1, bytes: 2}
	return projectDiagnosticSnapshotObject(fields, diagnosticSnapshotSchema, 1, &budget)
}

type diagnosticSnapshotBudget struct {
	nodes int
	bytes int
}

func (b *diagnosticSnapshotBudget) take(nodes, size int) bool {
	if b.nodes+nodes > diagnosticSnapshotMaxNodes || b.bytes+size > diagnosticSnapshotMaxBytes {
		return false
	}
	b.nodes += nodes
	b.bytes += size
	return true
}

func projectDiagnosticSnapshotObject(input map[string]any, schema []diagnosticSnapshotField, depth int, budget *diagnosticSnapshotBudget) map[string]any {
	out := make(map[string]any)
	if depth > diagnosticSnapshotMaxDepth {
		return out
	}
	for _, field := range schema {
		value, present := input[field.key]
		if !present {
			continue
		}
		if len(field.children) > 0 {
			child, ok := value.(map[string]any)
			if !ok || !budget.take(1, len(field.key)+6) {
				continue
			}
			projected := projectDiagnosticSnapshotObject(child, field.children, depth+1, budget)
			if len(projected) > 0 {
				out[field.key] = projected
			}
			continue
		}
		projected, nodes, size, ok := projectDiagnosticSnapshotScalar(value, field)
		// Size includes the key, colon, commas and conservatively bounded scalar
		// JSON encoding. Only validated ASCII strings can reach this point.
		if ok && budget.take(nodes, len(field.key)+4+size) {
			out[field.key] = projected
		}
	}
	return out
}

func projectDiagnosticSnapshotScalar(value any, field diagnosticSnapshotField) (any, int, int, bool) {
	switch field.kind {
	case "bool":
		if b, ok := value.(bool); ok {
			return b, 1, 5, true
		}
	case "model":
		if s, ok := value.(string); ok && safeDiagnosticSnapshotModel(s) {
			return s, 1, len(s) + 2, true
		}
	case "enum":
		if s, ok := value.(string); ok && diagnosticSnapshotAllowed(s, field.allowed) {
			return s, 1, len(s) + 2, true
		}
	case "betas":
		// All-or-nothing: malformed/overbound lists do not retain prefixes.
		var items []string
		switch v := value.(type) {
		case []string:
			if len(v) == 0 || len(v) > 4 {
				break
			}
			items = append([]string(nil), v...)
		case []any:
			if len(v) == 0 || len(v) > 4 {
				break
			}
			items = make([]string, len(v))
			for i, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, 0, 0, false
				}
				items[i] = s
			}
		}
		if len(items) == 0 {
			break
		}
		size := 2
		for _, s := range items {
			if !diagnosticSnapshotAllowed(s, field.allowed) {
				return nil, 0, 0, false
			}
			size += len(s) + 3
		}
		return items, len(items) + 1, size, true
	case "count", "temperature", "probability":
		n, ok := diagnosticSnapshotNumber(value)
		if !ok || n < 0 {
			break
		}
		switch field.kind {
		case "count":
			if n <= diagnosticSnapshotMaxCount && math.Trunc(n) == n && diagnosticSnapshotExactInteger(value) {
				return int64(n), 1, 10, true
			}
		case "temperature":
			if n <= 2 && (n != 2 || diagnosticSnapshotExactInteger(value)) {
				return n, 1, 32, true
			}
		case "probability":
			if n <= 1 && (n != 1 || diagnosticSnapshotExactInteger(value)) {
				return n, 1, 32, true
			}
		}
	}
	return nil, 0, 0, false
}

func diagnosticSnapshotAllowed(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func safeDiagnosticSnapshotModel(value string) bool {
	if !safeDiagnosticLabel(value, 128) {
		return false
	}
	lower := strings.ToLower(value)
	for _, sensitive := range []string{"sk-", "bearer", "eyj", "ghp_", "gho_", "xox", "http:", "https:", "token", "secret", "password", "credential", "authorization", "cookie", "session", "ticket", "api_key", "apikey"} {
		if strings.Contains(lower, sensitive) {
			return false
		}
	}
	return true
}

// Check json.Number integrality before float conversion can round a fractional
// lexeme (e.g. 1.00000000000000001) into a whole number. Work stays bounded by
// diagnosticSnapshotNumber's 32-byte numeric limit; exponents are never expanded.
func diagnosticSnapshotExactInteger(value any) bool {
	n, ok := value.(json.Number)
	if !ok {
		return true
	}
	mantissa := strings.TrimPrefix(string(n), "-")
	exponent := int64(0)
	if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(mantissa[i+1:], 10, 16)
		if err != nil {
			return false
		}
		mantissa = mantissa[:i]
	}
	fractionDigits := 0
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fractionDigits = len(mantissa) - i - 1
		mantissa = mantissa[:i] + mantissa[i+1:]
	}
	remaining := int64(fractionDigits) - exponent
	for i := len(mantissa) - 1; i >= 0 && remaining > 0; i-- {
		if mantissa[i] != '0' {
			return false
		}
		remaining--
	}
	return true
}

// Unlike legacy scalar metadata, snapshots never coerce strings into numbers.
func diagnosticSnapshotNumber(value any) (float64, bool) {
	var n float64
	switch v := value.(type) {
	case int:
		n = float64(v)
	case int8:
		n = float64(v)
	case int16:
		n = float64(v)
	case int32:
		n = float64(v)
	case int64:
		n = float64(v)
	case uint:
		n = float64(v)
	case uint8:
		n = float64(v)
	case uint16:
		n = float64(v)
	case uint32:
		n = float64(v)
	case uint64:
		n = float64(v)
	case float32:
		n = float64(v)
	case float64:
		n = v
	case json.Number:
		if len(v) == 0 || len(v) > 32 || v[0] == '-' || !json.Valid([]byte(v)) {
			return 0, false
		}
		var err error
		n, err = strconv.ParseFloat(string(v), 64)
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}

// ProjectRequestDiagnosticFields is the storage boundary for the single admitted
// diagnostic event. Only scalar metadata and explicitly projected snapshots survive.
func ProjectRequestDiagnosticFields(fields map[string]any) map[string]any {
	out := SafeRequestDiagnosticMetadata(fields)
	out["component"] = RequestDiagnosticsComponent
	out["event"] = MessagesWSLatencyEvent
	for key, allowed := range map[string][]string{
		"ingress":   {"messages_http"},
		"transport": {"websocket"},
		"outcome":   {"completed", "http_fallback", "canceled", "failed"},
	} {
		if value, ok := fields[key].(string); ok {
			for _, label := range allowed {
				if value == label {
					out[key] = value
					break
				}
			}
		}
	}
	for _, key := range []string{"conn_reused", "force_new_conn"} {
		if value, ok := fields[key].(bool); ok {
			out[key] = value
		}
	}
	for _, key := range []string{
		"attempt_duration_ms", "prep_ms", "acquire_ms", "prewrite_ms", "write_ms",
		"upstream_first_event_wait_ms", "upstream_first_semantic_wait_ms",
		"first_text_ms", "first_semantic_ms", "conn_pick_ms", "queue_wait_ms",
	} {
		if value, ok := diagnosticInteger(fields[key]); ok && value >= 0 && value <= 86400000 {
			out[key] = value
		}
	}
	for key, value := range ProjectRequestDiagnosticSnapshots(fields) {
		out[key] = value
	}
	return out
}
