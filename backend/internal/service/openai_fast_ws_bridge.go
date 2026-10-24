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

// resolveOpenAIFastWSBridgeDecision allows eligible HTTP ingress to use the
// Responses WebSocket. The configured scope selects all tiers or Fast only;
// transport selection never changes the final tier produced by policy.
// Only ChatGPT OAuth/Codex accounts participate in this optional bridge.
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
	if keepOpenAIHTTPIngressOnHTTP(ctx, c, account) {
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
	settings := s.openAIHTTPWSBridgeSettings(ctx)
	if !settings.Enabled {
		return baseDecision, false
	}
	// Evaluate the tier against the same upstream model Forward patches into
	// the body (account mapping + Codex normalization), not the raw mapping.
	_, upstreamModel := resolveOpenAIForwardMappedModels(account, model, false)

	tier := s.effectiveOpenAIServiceTierForTransport(ctx, account, upstreamModel, body)
	if !openAIHTTPWSBridgeScopeAllowsTier(settings.Scope, tier) {
		return baseDecision, false
	}

	if baseDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		reason := "fast_tier_http_bridge"
		if settings.Scope == OpenAIFastBridgeScopeAll {
			reason = "all_tiers_http_bridge"
		}
		return OpenAIWSProtocolDecision{
			Transport: OpenAIUpstreamTransportResponsesWebsocketV2,
			Reason:    reason,
		}, true
	}

	// A group-level forced Fast/Ultra Fast is an explicit administrator intent.
	// Honor it even when the account-level WS switch is off, as long as global
	// WS is usable; if the upstream WS dial fails, Forward falls back to HTTP so
	// the request still completes (at standard tier) instead of failing.
	if fastTierRequiresWSUpstream(openAIGroupForcedServiceTier(ctx, account, upstreamModel)) &&
		fastTierRequiresWSUpstream(tier) &&
		s.openAIFastBridgeMayOverrideAccountSwitch(account, baseDecision.Reason) {
		return OpenAIWSProtocolDecision{
			Transport: OpenAIUpstreamTransportResponsesWebsocketV2,
			Reason:    "fast_tier_group_forced_ws",
		}, true
	}
	return baseDecision, false
}

// openAIHTTPWSBridgeSettings reads one enabled/scope snapshot. Web settings are
// the single source of truth; an unavailable settings service keeps HTTP.
func (s *OpenAIGatewayService) openAIHTTPWSBridgeSettings(ctx context.Context) OpenAIFastBridgeSettings {
	if s == nil || s.settingService == nil {
		return *DefaultOpenAIFastBridgeSettings()
	}
	return s.settingService.GetOpenAIFastBridgeSettingsCached(ctx)
}

func (s *OpenAIGatewayService) openAIFastBridgeEnabled(ctx context.Context) bool {
	return s.openAIHTTPWSBridgeSettings(ctx).Enabled
}

func openAIHTTPWSBridgeScopeAllowsTier(scope, tier string) bool {
	return scope == OpenAIFastBridgeScopeAll ||
		(scope == OpenAIFastBridgeScopeFastOnly && fastTierRequiresWSUpstream(tier))
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
