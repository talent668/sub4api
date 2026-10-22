package service

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// fastTierRequiresWSUpstream reports whether tier must be scheduled by the
// ChatGPT Codex backend over the Responses WebSocket transport. The private
// Codex backend accepts service_tier=priority on HTTP POST /responses but only
// schedules (and bills) priority/ultrafast when the turn rides the WebSocket
// transport; the HTTP response echo stays default/auto either way.
func fastTierRequiresWSUpstream(tier string) bool {
	switch tier {
	case OpenAIFastTierPriority, OpenAIFastTierUltrafast:
		return true
	default:
		return false
	}
}

// resolveOpenAIFastWSBridgeDecision overrides the client-transport gate for
// HTTP/SSE ingress when the request would otherwise be served at a fast tier
// that the ChatGPT Codex HTTP endpoint silently downgrades to standard.
//
// The official Codex CLI always talks to the ChatGPT backend over the Responses
// WebSocket, which is why Fast works there but not for HTTP/SSE downstreams
// relayed through sub4api. The upstream WS branch in Forward already writes the
// downstream response (SSE or JSON), so an HTTP ingress request can safely use
// the WS upstream as long as the final payload is the one the policy produced.
//
// Only ChatGPT OAuth/Codex accounts are eligible: platform API-key HTTP
// requests already get priority scheduling over HTTP.
func (s *OpenAIGatewayService) resolveOpenAIFastWSBridgeDecision(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	baseDecision OpenAIWSProtocolDecision,
) (OpenAIWSProtocolDecision, bool) {
	if s == nil || account == nil || c == nil {
		return baseDecision, false
	}
	if GetOpenAIClientTransport(c) != OpenAIClientTransportHTTP {
		return baseDecision, false
	}
	if !account.UsesOpenAICodexProtocol() {
		return baseDecision, false
	}
	if s.cfg == nil || !s.cfg.Gateway.OpenAIWS.FastBridgeEnabled {
		// Opt-in: keep the pre-existing HTTP forwarding unless the operator asks
		// to test the HTTP->WS fast path.
		return baseDecision, false
	}
	if isOpenAIResponsesCompactPath(c) {
		return baseDecision, false
	}
	if isOpenAICompatMessagesBridgeBody(body) {
		return baseDecision, false
	}

	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		return baseDecision, false
	}
	mappedModel := account.GetMappedModel(model)

	tier := s.effectiveOpenAIServiceTierForTransport(ctx, account, mappedModel, body)
	if !fastTierRequiresWSUpstream(tier) {
		return baseDecision, false
	}

	if baseDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		return OpenAIWSProtocolDecision{
			Transport: OpenAIUpstreamTransportResponsesWebsocketV2,
			Reason:    "fast_tier_http_bridge",
		}, true
	}

	// A group-level forced Fast/Ultra Fast is an explicit administrator intent.
	// Honor it even when the account-level WS switch is off, as long as global
	// WS is usable; if the upstream WS dial fails, Forward falls back to HTTP so
	// the request still completes (at standard tier) instead of failing.
	if openAIGroupForcedServiceTier(ctx, account, mappedModel) != "" &&
		s.openAIFastBridgeMayOverrideAccountSwitch(account, baseDecision.Reason) {
		return OpenAIWSProtocolDecision{
			Transport: OpenAIUpstreamTransportResponsesWebsocketV2,
			Reason:    "fast_tier_group_forced_ws",
		}, true
	}
	return baseDecision, false
}

// openAIFastBridgeMayOverrideAccountSwitch reports whether a group-forced fast
// tier may ignore an account-level WS opt-out. Global hard switches and an
// explicit per-account force_http are always respected.
func (s *OpenAIGatewayService) openAIFastBridgeMayOverrideAccountSwitch(account *Account, reason string) bool {
	if s == nil || s.cfg == nil || account == nil {
		return false
	}
	wsCfg := s.cfg.Gateway.OpenAIWS
	if wsCfg.ForceHTTP || !wsCfg.Enabled || !wsCfg.OAuthEnabled || !wsCfg.ResponsesWebsocketsV2 {
		return false
	}
	if account.Concurrency <= 0 {
		return false
	}
	switch reason {
	case "account_disabled", "account_mode_off", "ws_v2_mode_http_bridge":
		return true
	default:
		return false
	}
}

// effectiveOpenAIServiceTierForTransport resolves the tier that Forward will
// send upstream, mirroring the later body patch order without mutating the
// body: group force first, then the client value, then the global policy, then
// the missing-tier force switch.
func (s *OpenAIGatewayService) effectiveOpenAIServiceTierForTransport(
	ctx context.Context,
	account *Account,
	model string,
	body []byte,
) string {
	tier := openAIGroupForcedServiceTier(ctx, account, model)
	if tier == "" {
		tier = normalizedOpenAIServiceTierValue(gjson.GetBytes(body, "service_tier").String())
	}
	if tier == "" {
		if s.shouldForceOpenAIFastPriorityForMissingTier(ctx, account, model) {
			tier = OpenAIFastTierPriority
		}
	}
	if tier == "" {
		return ""
	}
	action, _ := s.evaluateOpenAIFastPolicy(ctx, account, model, tier)
	switch action {
	case BetaPolicyActionFilter:
		return ""
	case OpenAIFastPolicyActionForcePriority:
		return OpenAIFastTierPriority
	default:
		return normalizedOpenAIServiceTierValue(tier)
	}
}
