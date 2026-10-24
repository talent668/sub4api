package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// openAIFastWSBridgeServerAvailable reports whether the global WS switches allow
// the fast bridge to dial the upstream Responses WebSocket. The account-level WS
// switch is intentionally not evaluated here: a group-forced Fast tier is an
// explicit administrator intent and may override an account-level opt-out (the
// hard global switches still win).
func (s *OpenAIGatewayService) openAIFastWSBridgeServerAvailable() bool {
	if s == nil || s.cfg == nil {
		return false
	}
	wsCfg := s.cfg.Gateway.OpenAIWS
	return !wsCfg.ForceHTTP && wsCfg.Enabled && wsCfg.OAuthEnabled && wsCfg.ResponsesWebsocketsV2
}

// resolveAnthropicMessagesWSBridge decides whether an Anthropic Messages request
// served by a ChatGPT OAuth/Codex account should use the upstream Responses WSv2
// instead of the HTTP compat path.
//
// The scope can include Standard as well as Fast. Read the tier from the final
// outbound body: group/client tiers and missing-tier policies have already been
// applied, and bridge selection must not evaluate or rewrite that policy again.
//
// Semantics mirror resolveOpenAIFastWSBridgeDecision: a client-sent fast tier
// still respects the account-level WS switch, while a group-forced fast tier is
// an explicit administrator intent that may override an account-level opt-out
// (the hard global switches still apply).
func (s *OpenAIGatewayService) resolveAnthropicMessagesWSBridge(
	ctx context.Context,
	account *Account,
	upstreamModel string,
	body []byte,
) (OpenAIWSProtocolDecision, bool) {
	base := OpenAIWSProtocolDecision{}
	if s == nil || account == nil || len(body) == 0 {
		return base, false
	}
	if keepOpenAIHTTPIngressOnHTTP(ctx, nil, account) {
		return base, false
	}
	if account.Platform != PlatformOpenAI || !account.UsesOpenAICodexProtocol() {
		return base, false
	}
	// Passthrough accounts keep their own instructions/namespace contract; the
	// shared WS branch would silently change those semantics.
	if account.IsOpenAIPassthroughEnabled() {
		return base, false
	}
	settings := s.openAIHTTPWSBridgeSettings(ctx)
	if !settings.Enabled {
		return base, false
	}
	tier := normalizedOpenAIServiceTierValue(gjson.GetBytes(body, "service_tier").String())
	if !openAIHTTPWSBridgeScopeAllowsTier(settings.Scope, tier) {
		return base, false
	}
	base = s.getOpenAIWSProtocolResolver().Resolve(account)
	if base.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		return base, true
	}
	if fastTierRequiresWSUpstream(openAIGroupForcedServiceTier(ctx, account, upstreamModel)) &&
		fastTierRequiresWSUpstream(tier) &&
		s.openAIFastBridgeMayOverrideAccountSwitch(account, base.Reason) {
		return OpenAIWSProtocolDecision{
			Transport: OpenAIUpstreamTransportResponsesWebsocketV2,
			Reason:    "fast_tier_messages_group_forced_ws",
		}, true
	}
	return base, false
}

// forwardAnthropicMessagesViaFastWS runs one Anthropic Messages request through
// the upstream Responses WSv2 forwarder and re-encodes the WS events as Anthropic
// SSE (streaming clients) or Anthropic Messages JSON (non-streaming clients).
//
// handled=false means the WS attempt failed before any downstream byte was
// written; the caller must fall back to the original HTTP path so the request
// still completes (at standard scheduling).
func (s *OpenAIGatewayService) forwardAnthropicMessagesViaFastWS(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	token string,
	reqBody map[string]any,
	promptCacheKey string,
	decision OpenAIWSProtocolDecision,
	isCodexCLI bool,
	clientStream bool,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
	reasoningEffort string,
	firstOutputTimeout time.Duration,
) (outResult *OpenAIForwardResult, outErr error, handled bool) {
	// Capture only request correlation metadata before starting the WS producer.
	fields := []zap.Field{zap.String("platform", PlatformOpenAI), zap.String("model", upstreamModel)}
	if account != nil && account.ID > 0 {
		fields = append(fields, zap.Int64("account_id", account.ID))
	}
	if apiKeyID := getAPIKeyIDFromContext(c); apiKeyID > 0 {
		fields = append(fields, zap.Int64("api_key_id", apiKeyID))
	}
	if userID, ok := ctx.Value(ctxkey.UserID).(int64); ok && userID > 0 {
		fields = append(fields, zap.Int64("user_id", userID))
	}
	for _, key := range []ctxkey.Key{ctxkey.RequestID, ctxkey.ClientRequestID} {
		if value, ok := ctx.Value(key).(string); ok && value != "" {
			name := "request_id"
			if key == ctxkey.ClientRequestID {
				name = "client_request_id"
			}
			fields = append(fields, zap.String(name, value))
		}
	}
	ctx = logger.IntoContext(ctx, logger.FromContext(ctx).With(fields...))
	latency := &openAIWSLatency{start: startTime, diagnostic: newOpenAIWSDiagnosticMetadata(ctx)}
	defer func() { latency.log(ctx, outResult, outErr, handled) }()
	ctx = context.WithValue(ctx, openAIWSLatencyContextKey{}, latency)
	// Both client modes need the full event stream: buffered Messages replies
	// recover missing terminal text/tool arguments from the preceding deltas.
	wsCtx, stopWS := context.WithCancelCause(ctx)
	defer stopWS(nil)
	pr, pw := io.Pipe()
	firstWrite := make(chan struct{})
	var firstWriteOnce sync.Once
	sink := newOpenAIWSPipeDownstream(pw, func() {
		firstWriteOnce.Do(func() { close(firstWrite) })
	})

	// Keep the shared WS pool/state store in the same account/API-key namespace
	// as HTTP Messages continuation, even when clients reuse a raw session seed.
	sessionSeed := strings.TrimSpace(promptCacheKey)
	if sessionSeed == "" {
		sessionSeed = s.GenerateSessionHash(c, nil)
	}
	executionScope, _ := deriveOpenAISessionHashes(openAICompatSessionResponseKey(c, account, sessionSeed))
	agentTaskRecoveryTried := false
	done := make(chan struct{})
	var wsResult *OpenAIForwardResult
	var wsErr error
	go func() {
		defer close(done)
		wsResult, wsErr = s.forwardOpenAIWSV2(
			wsCtx,
			c,
			account,
			reqBody,
			promptCacheKey,
			executionScope,
			token,
			decision,
			isCodexCLI,
			true,
			originalModel,
			upstreamModel,
			startTime,
			1,
			"",
			&agentTaskRecoveryTried,
			firstOutputTimeout,
			reasoningEffort,
			sink,
		)
		if wsErr != nil {
			_ = pw.CloseWithError(wsErr)
			return
		}
		_ = pw.Close()
	}()

	// Wait until the upstream produced its first downstream frame (or failed).
	// Starting the Anthropic converter only after the first frame keeps a WS
	// failure HTTP-fallback-safe: nothing has been written to the client yet.
	select {
	case <-firstWrite:
	case <-done:
		_ = pr.Close()
		// A canceled client can leave no pipe frames while the WS forwarder
		// still drains a complete, billable turn. Never replay that turn over HTTP.
		if wsResult != nil && wsResult.ClientDisconnect {
			wsResult.Model = originalModel
			wsResult.BillingModel = billingModel
			wsResult.Stream = clientStream
			applyOpenAIWSBridgeResultFields(wsResult, wsResult, sink)
			return wsResult, wsErr, true
		}
		if ctx.Err() != nil {
			return wsResult, ctx.Err(), true
		}
		// 首个语义输出超时意味着截止时间已经过去：HTTP 兜底会立刻再次超时，
		// 与 Forward 一致直接交给上层切换账号。
		var failoverErr *UpstreamFailoverError
		if errors.As(wsErr, &failoverErr) {
			return nil, wsErr, true
		}
		return nil, nil, false
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     sink.HeaderSnapshot(),
		Body:       pr,
	}
	var result *OpenAIForwardResult
	var convErr error
	if clientStream {
		result, convErr = s.handleAnthropicStreamingResponse(resp, c, account, originalModel, billingModel, upstreamModel, startTime)
	} else {
		result, convErr = s.handleAnthropicBufferedStreamingResponse(resp, c, account, originalModel, billingModel, upstreamModel, startTime)
	}
	// An adapter timeout must interrupt a pending WS read as well as pipe writes.
	// Actual client cancellation keeps the forwarder's detached usage drain.
	if convErr != nil && ctx.Err() == nil {
		stopWS(errOpenAIWSBridgeConverterStopped)
	}
	_ = pr.CloseWithError(io.ErrClosedPipe)
	<-done

	if result == nil && wsResult != nil && wsResult.ClientDisconnect {
		// The buffered converter cannot build a reply without a terminal frame,
		// but the detached WS drain may still have complete usage for billing.
		result = wsResult
		result.Model = originalModel
		result.BillingModel = billingModel
		result.Stream = clientStream
	}
	applyOpenAIWSBridgeResultFields(result, wsResult, sink)
	if convErr != nil {
		return result, convErr, true
	}
	if wsErr != nil {
		return result, wsErr, true
	}
	return result, nil, true
}

// errOpenAIWSBridgeConverterStopped distinguishes adapter failure from a real
// client disconnect, for which the WS forwarder must continue draining usage.
var errOpenAIWSBridgeConverterStopped = errors.New("messages WS converter stopped")

// applyOpenAIWSBridgeResultFields merges the WS attempt metadata into the result
// produced by the Anthropic converter so usage/ops can tell this request went
// over the upstream WebSocket.
func applyOpenAIWSBridgeResultFields(result, wsResult *OpenAIForwardResult, sink *openAIWSDownstreamSink) {
	if result == nil {
		return
	}
	result.OpenAIWSMode = true
	if sink != nil {
		if headers := sink.HeaderSnapshot(); len(headers) > 0 {
			result.UpstreamHeaders = headers
			result.ResponseHeaders = headers
		}
	}
	if wsResult == nil {
		return
	}
	if result.ResponseID == "" {
		result.ResponseID = wsResult.ResponseID
	}
	// WS usage deduplicates by RequestID. A handshake x-request-id is shared
	// across turns on a pooled connection, so use the forwarder's response ID.
	if wsResult.RequestID != "" {
		result.RequestID = wsResult.RequestID
	} else if wsResult.ResponseID != "" {
		result.RequestID = wsResult.ResponseID
	}
	if result.ResponseHeaders == nil {
		result.ResponseHeaders = wsResult.ResponseHeaders
	}
	if result.UpstreamHeaders == nil {
		result.UpstreamHeaders = wsResult.ResponseHeaders
	}
	result.FirstTokenMs = wsResult.FirstTokenMs
	// The forwarder observes raw frames before client-facing model rewriting
	// and can continue reading after the converter has stopped. Its terminal
	// usage and observation are authoritative, not additive billing snapshots.
	if openAIUsageHasTokens(&wsResult.Usage) {
		result.Usage = wsResult.Usage
	}
	result.ClientDisconnect = result.ClientDisconnect || wsResult.ClientDisconnect
	result.UpstreamResponseModel = wsResult.UpstreamResponseModel
	result.UpstreamResponseModelConflict = wsResult.UpstreamResponseModelConflict
	result.UpstreamResponseServiceTier = wsResult.UpstreamResponseServiceTier
	result.ServiceTier = wsResult.ServiceTier
	result.UpstreamTerminalEvent = wsResult.UpstreamTerminalEvent
}

// injectCompatTurnStateForWSBridge exposes the stored compat turn state to the
// WS forwarder (which reads it from the request header, like the native WS
// ingress) and returns a restore function.
func injectCompatTurnStateForWSBridge(c *gin.Context, turnState string) func() {
	noop := func() {}
	if c == nil || c.Request == nil {
		return noop
	}
	turnState = strings.TrimSpace(turnState)
	if turnState == "" {
		return noop
	}
	previous := c.Request.Header.Get(openAIWSTurnStateHeader)
	c.Request.Header.Set(openAIWSTurnStateHeader, turnState)
	return func() {
		if previous == "" {
			c.Request.Header.Del(openAIWSTurnStateHeader)
			return
		}
		c.Request.Header.Set(openAIWSTurnStateHeader, previous)
	}
}

// finalizeAnthropicMessagesWSResult replays the post-response bookkeeping the
// HTTP compat path performs after upstream headers arrive (turn state, response
// id continuation, service tier echo, Codex rate-limit snapshot).
func (s *OpenAIGatewayService) finalizeAnthropicMessagesWSResult(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	result *OpenAIForwardResult,
	responsesBody []byte,
	promptCacheKey string,
	anthropicDigestChain string,
	anthropicMatchedDigestChain string,
	apiKeyID int64,
	compatContinuationEnabled bool,
	reasoningEffort string,
) {
	if result == nil {
		return
	}
	if result.ResponseHeaders != nil {
		if turnState := strings.TrimSpace(result.ResponseHeaders.Get(openAIWSTurnStateHeader)); turnState != "" && promptCacheKey != "" {
			s.bindOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey, turnState)
		}
		if account != nil && account.Type == AccountTypeOAuth && !account.IsShadow() && account.Platform != PlatformGrok {
			if snapshot := ParseCodexRateLimitHeaders(result.ResponseHeaders); snapshot != nil {
				s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
			}
		}
	}
	if compatContinuationEnabled && promptCacheKey != "" && result.ResponseID != "" {
		s.bindOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey, result.ResponseID)
	}
	if promptCacheKey != "" && anthropicDigestChain != "" {
		s.bindOpenAICompatAnthropicDigestPromptCacheKey(account, apiKeyID, anthropicDigestChain, promptCacheKey, anthropicMatchedDigestChain)
	}
	if tier := resolvedOpenAIUpstreamServiceTier(c, extractOpenAIServiceTierFromBody(responsesBody)); tier != nil {
		result.ServiceTier = tier
	}
	if strings.TrimSpace(reasoningEffort) != "" {
		value := reasoningEffort
		result.ReasoningEffort = &value
	}
	if c != nil {
		c.Set("openai_ws_transport_decision", string(OpenAIUpstreamTransportResponsesWebsocketV2))
		c.Set("openai_ws_transport_reason", "fast_tier_messages_bridge")
	}
	stampOpenAIResponsesUpstreamEndpoint(c, result)
}
