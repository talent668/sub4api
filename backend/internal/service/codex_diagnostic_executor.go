package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// codexDiagnosticRequestTimeout bounds a single model probe. It matches the
// interactive diagnostic endpoint's per-model deadline.
const codexDiagnosticRequestTimeout = 120 * time.Second

const codexDiagnosticBodyLimit = 2 << 20

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
	defer func() { result.LatencyMs = time.Since(startedAt).Milliseconds() }()

	probeCtx, err := e.gateway.PrepareCodexProbeContext(ctx)
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

	modelCtx, cancel := context.WithTimeout(WithCodexTicketDiagnostic(probeCtx, accountID), codexDiagnosticRequestTimeout)
	request, err := http.NewRequestWithContext(modelCtx, http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		cancel()
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

	writer := httptest.NewRecorder()
	router.ServeHTTP(writer, request)
	cancel()

	result.HTTPStatus = writer.Code
	if writer.Code < 200 || writer.Code >= 300 {
		result.Reason = "gateway_request_failed"
		result.GatewayErrorCode = CodexDiagnosticGatewayError(writer.Body.Bytes())
		return
	}

	output, readErr := io.ReadAll(io.LimitReader(writer.Body, codexDiagnosticBodyLimit))
	if readErr != nil {
		result.Reason = "response_read_failed"
		return
	}
	diagnosticText, complete := CodexDiagnosticOutput(output)
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
	if len(text) <= maxReplyChars {
		return text
	}
	return text[:maxReplyChars]
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
	var output strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), codexDiagnosticBodyLimit)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		streamed = true
		switch gjson.Get(payload, "type").String() {
		case "response.output_text.delta":
			_, _ = output.WriteString(gjson.Get(payload, "delta").String())
		case "response.completed":
			completed = true
			if output.Len() == 0 {
				gjson.Get(payload, "response.output").ForEach(func(_, message gjson.Result) bool {
					message.Get("content").ForEach(func(_, part gjson.Result) bool {
						if part.Get("type").String() == "output_text" {
							_, _ = output.WriteString(part.Get("text").String())
						}
						return true
					})
					return true
				})
			}
		}
	}
	if streamed {
		return output.String(), completed && scanner.Err() == nil
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return "", false
	}
	if gjson.GetBytes(raw, "status").String() != "completed" {
		return "", false
	}
	if text := gjson.GetBytes(raw, "output_text").String(); text != "" {
		return text, true
	}
	return gjson.GetBytes(raw, "output.0.content.0.text").String(), true
}
