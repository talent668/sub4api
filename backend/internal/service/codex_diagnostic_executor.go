package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// codexDiagnosticRequestTimeout bounds a single model probe. It matches the
// interactive diagnostic endpoint's per-model deadline.
const codexDiagnosticRequestTimeout = 120 * time.Second

const codexDiagnosticBodyLimit = 2 << 20

var errCodexDiagnosticResponseTooLarge = errors.New("diagnostic response exceeds size limit")

type codexDiagnosticRequestSourceKey struct{}

type codexDiagnosticRequestSource struct {
	remoteAddr string
	host       string
}

// WithCodexDiagnosticRequestSource preserves the interactive caller's origin so
// the normal gateway applies the same API-key IP restrictions as before.
func WithCodexDiagnosticRequestSource(ctx context.Context, remoteAddr, host string) context.Context {
	return context.WithValue(ctx, codexDiagnosticRequestSourceKey{}, codexDiagnosticRequestSource{remoteAddr, host})
}

// Bound memory while the gateway writes, not after it has buffered the response.
// Flush is intentionally a no-op: probes consume the completed local response.
type codexDiagnosticResponseWriter struct {
	header   http.Header
	body     bytes.Buffer
	code     int
	overflow bool
	cancel   context.CancelFunc
}

func newCodexDiagnosticResponseWriter(cancel context.CancelFunc) *codexDiagnosticResponseWriter {
	return &codexDiagnosticResponseWriter{header: make(http.Header), cancel: cancel}
}

func (w *codexDiagnosticResponseWriter) Header() http.Header { return w.header }

func (w *codexDiagnosticResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *codexDiagnosticResponseWriter) Write(body []byte) (int, error) {
	if w.overflow {
		return 0, errCodexDiagnosticResponseTooLarge
	}
	w.WriteHeader(http.StatusOK)
	remaining := codexDiagnosticBodyLimit - w.body.Len()
	if len(body) > remaining {
		n, _ := w.body.Write(body[:remaining])
		w.overflow = true
		if w.cancel != nil {
			w.cancel()
		}
		return n, errCodexDiagnosticResponseTooLarge
	}
	return w.body.Write(body)
}

func (w *codexDiagnosticResponseWriter) Flush() { w.WriteHeader(http.StatusOK) }

// CodexDiagnosticModelResult is the outcome of one model probe.
type CodexDiagnosticModelResult struct {
	Model            string
	Status           string
	Reason           string
	PredictedModel   string
	Probability      *float64
	ParsedCount      int
	HTTPStatus       int
	GatewayErrorCode string
	ModelReply       string
	LatencyMs        int64
	StartedAt        time.Time
	FinishedAt       time.Time
}

// CodexDiagnosticExecutor runs the Codex degradation diagnostic through the
// normal gateway router (authentication, scheduling, ticket policy and billing
// included). Shared by the interactive admin endpoint and scheduled plans.
type CodexDiagnosticExecutor struct {
	mu      sync.RWMutex
	router  http.Handler
	gateway *OpenAIGatewayService
	apiKeys *APIKeyService
}

// NewCodexDiagnosticExecutor creates an executor. The router may be attached
// later with SetRouter because it is only available after the HTTP engine is
// built.
func NewCodexDiagnosticExecutor(gateway *OpenAIGatewayService, apiKeys *APIKeyService) *CodexDiagnosticExecutor {
	return &CodexDiagnosticExecutor{gateway: gateway, apiKeys: apiKeys}
}

// SetRouter attaches the gateway HTTP handler used to dispatch probes.
func (e *CodexDiagnosticExecutor) SetRouter(router http.Handler) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.router = router
}

func (e *CodexDiagnosticExecutor) currentRouter() http.Handler {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.router
}

// Run probes each model serially and returns one result per requested model.
// The returned error only covers setup failures (missing executor, unavailable
// API key); per-model failures are represented in the results.
func (e *CodexDiagnosticExecutor) Run(
	ctx context.Context,
	accountID int64,
	apiKeyID int64,
	models []string,
	generateChallenge func() (ModelTraceChallenge, error),
) ([]CodexDiagnosticModelResult, error) {
	if e == nil || e.gateway == nil || e.apiKeys == nil {
		return nil, fmt.Errorf("codex diagnostic executor is not configured")
	}
	router := e.currentRouter()
	if router == nil {
		return nil, fmt.Errorf("codex diagnostic router is not configured")
	}
	if accountID <= 0 {
		return nil, fmt.Errorf("invalid account id")
	}
	if apiKeyID <= 0 {
		return nil, fmt.Errorf("invalid api key id")
	}
	key, err := e.apiKeys.GetByID(ctx, apiKeyID)
	if err != nil || key == nil || !key.IsActive() || key.IsExpired() || key.IsQuotaExhausted() || key.Key == "" {
		return nil, fmt.Errorf("API key unavailable or not usable for diagnostics")
	}
	allowed, err := ModelTraceGPTModels()
	if err != nil {
		return nil, err
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, model := range allowed {
		allowedSet[model] = true
	}
	if generateChallenge == nil {
		generateChallenge = NewModelTraceChallenge
	}

	results := make([]CodexDiagnosticModelResult, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		result := CodexDiagnosticModelResult{Model: model, Status: ScheduledDiagnosticStatusFailed}
		if model == "" {
			result.Reason = "invalid_model"
			results = append(results, result)
			continue
		}
		if !allowedSet[model] {
			result.Reason = "unsupported_model"
			results = append(results, result)
			continue
		}
		if ctx.Err() != nil {
			// Interactive callers abort on cancellation: don't synthesize rows
			// for models that were never probed.
			break
		}
		e.probeModel(ctx, router, key, accountID, model, generateChallenge, &result)
		results = append(results, result)
	}
	return results, nil
}

func (e *CodexDiagnosticExecutor) probeModel(
	ctx context.Context,
	router http.Handler,
	key *APIKey,
	accountID int64,
	model string,
	generateChallenge func() (ModelTraceChallenge, error),
	result *CodexDiagnosticModelResult,
) {
	startedAt := time.Now()
	result.StartedAt = startedAt
	defer func() {
		result.FinishedAt = time.Now()
		result.LatencyMs = result.FinishedAt.Sub(startedAt).Milliseconds()
	}()
	modelCtx, cancel := context.WithTimeout(ctx, codexDiagnosticRequestTimeout)
	defer cancel()

	probeCtx, err := e.gateway.PrepareCodexProbeContext(modelCtx)
	if err != nil {
		result.Reason = "template_unavailable"
		if errors.Is(err, ErrCodexProbeTemplateInvalid) {
			result.Reason = "template_invalid"
		}
		return
	}
	challenge, err := generateChallenge()
	if err != nil {
		result.Reason = "challenge_generation_failed"
		return
	}
	body, probeHeaders, err := e.gateway.BuildCodexDiagnosticRequest(probeCtx, accountID, model, challenge)
	if err != nil {
		result.Reason = "request_build_failed"
		if errors.Is(err, ErrCodexProbeIdentity) {
			result.Reason = "identity_resolution_failed"
		}
		return
	}

	request, err := http.NewRequestWithContext(WithCodexTicketDiagnostic(probeCtx, accountID), http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		result.Reason = "request_build_failed"
		return
	}
	for name, values := range probeHeaders {
		request.Header[name] = values
	}
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.RemoteAddr = "127.0.0.1:0"
	request.Host = "localhost"
	if source, ok := ctx.Value(codexDiagnosticRequestSourceKey{}).(codexDiagnosticRequestSource); ok {
		request.RemoteAddr = source.remoteAddr
		request.Host = source.host
	}

	writer := newCodexDiagnosticResponseWriter(cancel)
	router.ServeHTTP(writer, request)
	result.HTTPStatus = writer.code
	if writer.overflow {
		result.Reason = "response_too_large"
		return
	}
	if modelCtx.Err() != nil {
		result.Reason = "request_canceled"
		if errors.Is(modelCtx.Err(), context.DeadlineExceeded) {
			result.Reason = "request_timeout"
		}
		return
	}
	if writer.code < 200 || writer.code >= 300 {
		result.Reason = "gateway_request_failed"
		result.GatewayErrorCode = CodexDiagnosticGatewayError(writer.body.Bytes())
		return
	}

	diagnosticText, complete := CodexDiagnosticOutput(writer.body.Bytes())
	if !complete {
		result.Reason = "response_incomplete"
		return
	}
	prediction, _, predictErr := ModelTracePredictCommitted(diagnosticText, challenge.ExpectedCount)
	result.ParsedCount = prediction.ParsedCount
	if predictErr != nil {
		result.Reason = "insufficient_numbers"
		return
	}
	probability := prediction.Probability
	result.PredictedModel = prediction.Model
	result.Probability = &probability
	result.ModelReply = truncateScheduledDiagnosticReply(diagnosticText)
	if prediction.Model == model {
		result.Status = ScheduledDiagnosticStatusNormal
		return
	}
	result.Status = ScheduledDiagnosticStatusDegraded
	result.Reason = "fingerprint_mismatch"
}

func truncateScheduledDiagnosticReply(text string) string {
	const maxReplyChars = 500
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= maxReplyChars {
		return text
	}
	return string(runes[:maxReplyChars])
}

// CodexDiagnosticGatewayError extracts a safe diagnostic error code from a
// gateway error payload.
func CodexDiagnosticGatewayError(raw []byte) string {
	if len(raw) > 16<<10 {
		raw = raw[:16<<10]
	}
	for _, path := range []string{"reason", "error.code"} {
		code := gjson.GetBytes(raw, path).String()
		if code == "" || len(code) > 64 {
			continue
		}
		for _, character := range code {
			if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' {
				continue
			}
			code = ""
			break
		}
		if code != "" {
			return code
		}
	}
	return ""
}

// CodexDiagnosticOutput extracts the model reply from the gateway response and
// reports whether a complete terminal response was observed.
func CodexDiagnosticOutput(raw []byte) (string, bool) {
	completed := false
	streamed := false
	terminal := false
	valid := true
	var output strings.Builder
	forEachOpenAISSEFrame(string(raw), func(eventType string, payload []byte) {
		streamed = true
		if terminal {
			return
		}
		if !gjson.ValidBytes(payload) {
			valid = false
			return
		}
		switch eventType {
		case "response.output_text.delta":
			_, _ = output.WriteString(gjson.GetBytes(payload, "delta").String())
		case "response.completed", "response.done":
			terminal = true
			status := gjson.GetBytes(payload, "response.status").String()
			completed = status == "" || status == "completed"
			if completed && output.Len() == 0 {
				_, _ = output.WriteString(codexDiagnosticResponseText(gjson.GetBytes(payload, "response")))
			}
		case "error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			terminal = true
		}
	})
	if streamed {
		return output.String(), completed && valid
	}
	if !json.Valid(raw) || gjson.GetBytes(raw, "status").String() != "completed" {
		return "", false
	}
	return codexDiagnosticResponseText(gjson.ParseBytes(raw)), true
}

func codexDiagnosticResponseText(response gjson.Result) string {
	if text := response.Get("output_text").String(); text != "" {
		return text
	}
	var output strings.Builder
	response.Get("output").ForEach(func(_, message gjson.Result) bool {
		message.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "output_text" {
				_, _ = output.WriteString(part.Get("text").String())
			}
			return true
		})
		return true
	})
	return output.String()
}
