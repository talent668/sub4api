package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

const OpenAINativeWSOnlyExtraKey = "openai_native_ws_only"
const openAINativeWSRequiredContextKey = "openai_native_ws_required"

type nativeWSRequiredRequestContextKey struct{}

func isOpenAINativeWSRequired(c *gin.Context, a *Account) bool {
	if a.IsOpenAINativeWSOnly() {
		return true
	}
	if c == nil {
		return false
	}
	value, _ := c.Get(openAINativeWSRequiredContextKey)
	required, _ := value.(bool)
	return required
}

func (a *Account) IsOpenAINativeWSOnly() bool {
	if a == nil || a.Platform != PlatformOpenAI {
		return false
	}
	value, _ := a.Extra[OpenAINativeWSOnlyExtraKey].(bool)
	return value
}

func ValidateOpenAINativeWSPresetExtra(platform, accountType string, extra map[string]any) error {
	for _, key := range []string{OpenAINativeWSOnlyExtraKey, OpenAINativeWSPreferredExtraKey} {
		if value, exists := extra[key]; exists {
			if _, ok := value.(bool); !ok {
				return errors.New("native WS preset must be a boolean")
			}
		}
	}
	only, _ := extra[OpenAINativeWSOnlyExtraKey].(bool)
	preferred, _ := extra[OpenAINativeWSPreferredExtraKey].(bool)
	if only && preferred {
		return errors.New("native WS only and native WS preferred presets are mutually exclusive")
	}
	if !only && !preferred {
		return nil
	}
	a := &Account{Platform: platform, Type: accountType, Extra: extra}
	if platform != PlatformOpenAI || (!a.IsOpenAIOAuthLike() && !a.IsOpenAIApiKey()) {
		return errors.New("native WS preset requires an OpenAI OAuth or API key account")
	}
	if a.IsOpenAIWSForceHTTPEnabled() {
		return errors.New("native WS preset conflicts with forced HTTP")
	}
	defaultMode := OpenAIWSIngressModeCtxPool
	if preferred {
		// A preset must not depend on a deployment default that may be off,
		// passthrough, or http_bridge. The UI saves an explicit ctx_pool mode.
		defaultMode = OpenAIWSIngressModeOff
	}
	mode := a.ResolveOpenAIResponsesWebSocketV2Mode(defaultMode)
	if preferred && mode != OpenAIWSIngressModeCtxPool {
		return errors.New("native WS preferred preset requires ctx_pool mode")
	}
	if mode != OpenAIWSIngressModeCtxPool && mode != OpenAIWSIngressModePassthrough {
		return errors.New("native WS preset requires ctx_pool or passthrough")
	}
	return nil
}

func (s *OpenAIGatewayService) validateOpenAINativeWSOnly(a *Account) error {
	if a == nil || !a.IsOpenAI() || (!a.IsOpenAIOAuthLike() && !a.IsOpenAIApiKey()) {
		return errors.New("native WS requires a compatible OpenAI account")
	}
	if s == nil || s.cfg == nil || !s.cfg.Gateway.OpenAIWS.Enabled || s.cfg.Gateway.OpenAIWS.ForceHTTP || a.IsOpenAIWSForceHTTPEnabled() {
		return errors.New("native WS is disabled by the gateway or account")
	}
	cfg := s.cfg.Gateway.OpenAIWS
	if !cfg.ModeRouterV2Enabled || !cfg.ResponsesWebsocketsV2 {
		return errors.New("native WS preset requires mode_router_v2_enabled and responses_websockets_v2")
	}
	if (a.IsOpenAIOAuthLike() && !cfg.OAuthEnabled) || (a.IsOpenAIApiKey() && !cfg.APIKeyEnabled) {
		return errors.New("native WS is disabled for this account type")
	}
	mode := a.ResolveOpenAIResponsesWebSocketV2Mode(cfg.IngressModeDefault)
	if mode != OpenAIWSIngressModeCtxPool && mode != OpenAIWSIngressModePassthrough {
		return errors.New("native WS requires ctx_pool or passthrough mode")
	}
	if s.pluginManager != nil && s.pluginManager.ShouldRouteOpenAIOAuth(a) {
		return errors.New("native WS is incompatible with the account's HTTP transport plugin")
	}
	return nil
}

// RequireOpenAINativeWSIngress latches the policy across all account attempts
// on this client connection. It never changes another account's saved settings.
func (s *OpenAIGatewayService) RequireOpenAINativeWSIngress(c *gin.Context, a *Account) error {
	latchOpenAINativeWSPreferred(c, a)
	required := a.IsOpenAINativeWSOnly() || a.IsOpenAINativeWSPreferred()
	if c != nil {
		if value, ok := c.Get(openAINativeWSRequiredContextKey); ok {
			latched, _ := value.(bool)
			required = required || latched
		}
	}
	if !required {
		return nil
	}
	if c != nil {
		c.Set(openAINativeWSRequiredContextKey, true)
	}
	if err := s.validateOpenAINativeWSOnly(a); err != nil {
		return err
	}
	if keepOpenAIHTTPIngressOnHTTP(context.Background(), c, a) && a.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault) != OpenAIWSIngressModeCtxPool {
		return errors.New("native WS preferred preset requires ctx_pool mode")
	}
	return nil
}

func (s *OpenAIGatewayService) requireNativeWSClient(c *gin.Context, a *Account, protocol string) error {
	latchOpenAINativeWSPreferred(c, a)
	if !isOpenAINativeWSRequired(c, a) {
		return nil
	}
	if GetOpenAIClientTransport(c) == OpenAIClientTransportWS {
		return s.RequireOpenAINativeWSIngress(c, a)
	}
	err := errors.New("this account requires a native WebSocket client; HTTP bridging is disabled")
	if c != nil && c.Writer != nil && !c.Writer.Written() {
		if protocol == "anthropic" {
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		} else {
			writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
	}
	return err
}
