package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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
// The ChatGPT Codex backend only schedules priority/ultrafast on the Responses
// WebSocket; an HTTP Messages request carrying service_tier=priority is silently
// downgraded. The tier is read from the final outbound body because group-forced
// tiers, client-sent tiers and the missing-tier force switch have all been
// applied by applyOpenAIFastPolicyToBody before this point.
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
	if account.Platform != PlatformOpenAI || !account.UsesOpenAICodexProtocol() {
		return base, false
	}
	// Passthrough accounts keep their own instructions/namespace contract; the
	// shared WS branch would silently change those semantics.
	if account.IsOpenAIPassthroughEnabled() {
		return base, false
	}
	if !s.openAIFastBridgeEnabled(ctx) {
		return base, false
	}
	tier := normalizedOpenAIServiceTierValue(gjson.GetBytes(body, "service_tier").String())
	if !fastTierRequiresWSUpstream(tier) {
		return base, false
	}
	base = s.getOpenAIWSProtocolResolver().Resolve(account)
	if base.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		return base, true
	}
	if openAIGroupForcedServiceTier(ctx, account, upstreamModel) != "" &&
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
) (*OpenAIForwardResult, error, bool) {
	if !clientStream {
		return s.forwardAnthropicMessagesViaFastWSNonStream(
			ctx, c, account, token, reqBody, promptCacheKey, decision, isCodexCLI,
			originalModel, billingModel, upstreamModel, startTime, reasoningEffort,
		)
	}

	pr, pw := io.Pipe()
	firstWrite := make(chan struct{})
	var firstWriteOnce sync.Once
	sink := newOpenAIWSPipeDownstream(pw, func() {
		firstWriteOnce.Do(func() { close(firstWrite) })
	})

	agentTaskRecoveryTried := false
	done := make(chan struct{})
	var wsResult *OpenAIForwardResult
	var wsErr error
	go func() {
		defer close(done)
		wsResult, wsErr = s.forwardOpenAIWSV2(
			ctx,
			c,
			account,
			reqBody,
			promptCacheKey,
			"", // execution scope: session hash falls back to promptCacheKey
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
	result, convErr := s.handleAnthropicStreamingResponse(resp, c, account, originalModel, billingModel, upstreamModel, startTime)
	// The converter may return early (e.g. terminal event or fatal upstream
	// event); closing the read side makes any later WS write fail immediately
	// instead of blocking the forwarder forever.
	_ = pr.CloseWithError(io.ErrClosedPipe)
	<-done

	applyOpenAIWSBridgeResultFields(result, wsResult, sink)
	if convErr != nil {
		return result, convErr, true
	}
	if wsErr != nil && result == nil {
		return nil, wsErr, true
	}
	return result, nil, true
}

// forwardAnthropicMessagesViaFastWSNonStream buffers the upstream WS terminal
// response and converts it exactly like the HTTP compat path does: a synthetic
// terminal SSE event is fed to the existing buffered converter.
func (s *OpenAIGatewayService) forwardAnthropicMessagesViaFastWSNonStream(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	token string,
	reqBody map[string]any,
	promptCacheKey string,
	decision OpenAIWSProtocolDecision,
	isCodexCLI bool,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
	reasoningEffort string,
) (*OpenAIForwardResult, error, bool) {
	sink := newOpenAIWSCollectDownstream()
	agentTaskRecoveryTried := false
	wsResult, wsErr := s.forwardOpenAIWSV2(
		ctx,
		c,
		account,
		reqBody,
		promptCacheKey,
		"",
		token,
		decision,
		isCodexCLI,
		false,
		originalModel,
		upstreamModel,
		startTime,
		1,
		"",
		&agentTaskRecoveryTried,
		0,
		reasoningEffort,
		sink,
	)
	if wsErr != nil || wsResult == nil {
		// WS 在写出任何下游字节前失败：交回调用方走 HTTP。
		return nil, nil, false
	}
	status, _, body := sink.CollectedJSON()
	if len(body) == 0 {
		return nil, nil, false
	}
	if status != 0 && status != http.StatusOK {
		return nil, nil, false
	}
	eventType := "response.completed"
	switch strings.TrimSpace(gjson.GetBytes(body, "status").String()) {
	case "failed":
		eventType = "response.failed"
	case "incomplete":
		eventType = "response.incomplete"
	}
	var eventPayload bytes.Buffer
	eventPayload.WriteString(`{"type":"`)
	eventPayload.WriteString(eventType)
	eventPayload.WriteString(`","response":`)
	eventPayload.Write(body)
	eventPayload.WriteString(`}`)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     sink.HeaderSnapshot(),
		Body:       io.NopCloser(strings.NewReader("data: " + eventPayload.String() + "\n\n")),
	}
	result, convErr := s.handleAnthropicBufferedStreamingResponse(resp, c, account, originalModel, billingModel, upstreamModel, startTime)
	applyOpenAIWSBridgeResultFields(result, wsResult, sink)
	return result, convErr, true
}

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
	if result.RequestID == "" {
		result.RequestID = wsResult.RequestID
	}
	if result.ResponseHeaders == nil {
		result.ResponseHeaders = wsResult.ResponseHeaders
	}
	if result.UpstreamHeaders == nil {
		result.UpstreamHeaders = wsResult.ResponseHeaders
	}
	if result.FirstTokenMs == nil {
		result.FirstTokenMs = wsResult.FirstTokenMs
	}
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
