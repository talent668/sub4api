package service

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// fastTierRequiresWSUpstream reports whether the tier is eligible for the
// optional HTTP->WS bridge. HTTP Fast can take effect on its own in some
// environments; this helper only decides whether the bridge may be used when
// the operator enables it.
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
	// Passthrough accounts keep their passthrough HTTP contract (instructions
	// gate, no Codex transform, namespace restore on the way back); the shared
	// WS branch would silently change those semantics.
	if account.IsOpenAIPassthroughEnabled() {
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
	if !s.openAIFastBridgeEnabled(ctx) {
		// Opt-in: keep the pre-existing HTTP forwarding unless the operator
		// enables the web switch.
		return baseDecision, false
	}
	// Evaluate the tier against the same upstream model Forward patches into
	// the body (account mapping + Codex normalization), not the raw mapping.
	_, upstreamModel := resolveOpenAIForwardMappedModels(account, model, false)

	tier := s.effectiveOpenAIServiceTierForTransport(ctx, account, upstreamModel, body)
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
	if openAIGroupForcedServiceTier(ctx, account, upstreamModel) != "" &&
		s.openAIFastBridgeMayOverrideAccountSwitch(account, baseDecision.Reason) {
		return OpenAIWSProtocolDecision{
			Transport: OpenAIUpstreamTransportResponsesWebsocketV2,
			Reason:    "fast_tier_group_forced_ws",
		}, true
	}
	return baseDecision, false
}

// openAIFastBridgeEnabled resolves the web-only opt-in switch. It has no
// config/env fallback: the Admin Settings toggle is the single source of truth.
// The value is served from the setting service's in-process cache.
func (s *OpenAIGatewayService) openAIFastBridgeEnabled(ctx context.Context) bool {
	if s == nil || s.settingService == nil {
		return false
	}
	return s.settingService.IsOpenAIFastBridgeEnabledCached(ctx)
}

// openAIFastBridgeMayOverrideAccountSwitch reports whether a group-forced fast
// tier may ignore an account-level WS opt-out. Global hard switches and an
// explicit per-account force_http are always respected.
func (s *OpenAIGatewayService) openAIFastBridgeMayOverrideAccountSwitch(account *Account, reason string) bool {
	if s == nil || account == nil {
		return false
	}
	if !s.openAIFastWSBridgeServerAvailable() {
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

// effectiveOpenAIServiceTierForTransport resolves the tier that Forward's shared
// (non-passthrough) branch will send upstream, mirroring its patch order without
// mutating the body: a group-forced tier replaces the client value and goes
// through the global policy; a missing tier is only upgraded by the missing-tier
// force switch and that injected priority is final (Forward does not evaluate
// it again); an unrecognized client tier is left untouched.
func (s *OpenAIGatewayService) effectiveOpenAIServiceTierForTransport(
	ctx context.Context,
	account *Account,
	model string,
	body []byte,
) string {
	rawTier := openAIGroupForcedServiceTier(ctx, account, model)
	if rawTier == "" {
		rawTier = strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
	}
	if rawTier == "" {
		if s.shouldForceOpenAIFastPriorityForMissingTier(ctx, account, model) {
			return OpenAIFastTierPriority
		}
		return ""
	}
	normTier := normalizedOpenAIServiceTierValue(rawTier)
	if normTier == "" {
		return ""
	}
	action, _ := s.evaluateOpenAIFastPolicy(ctx, account, model, normTier)
	switch action {
	case BetaPolicyActionFilter:
		return ""
	case OpenAIFastPolicyActionForcePriority:
		return OpenAIFastTierPriority
	default:
		return normTier
	}
}
