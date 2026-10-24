package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageTransportDetailsProjectionAndUnknownHistory(t *testing.T) {
	require.Nil(t, NormalizeUsageTransportDetails(nil))
	require.Nil(t, ParseUsageTransportDetailsJSON(`{"client":"secret","upstream":"ws"}`))
	d := ParseUsageTransportDetailsJSON(`{"client":"ws","upstream":"http","mode":"http_bridge","fallback_reason":"large_payload","token":"secret"}`)
	require.Equal(t, newUsageTransportDetails("ws", "http", "http_bridge", "large_payload"), d)
	require.NotContains(t, UsageTransportDetailsJSON(d), "secret")
	d = ParseUsageTransportDetailsJSON(`{"client":"http","upstream":"ws","mode":"attacker-secret","fallback_reason":"private token"}`)
	require.Equal(t, newUsageTransportDetails("http", "ws", "", ""), d)
}

func TestUsageTransportObservesFinalRouteAndSeparatesAttempts(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	ctx, trace := withUsageTransportTrace(context.Background(), c)
	observeUsageTransport(ctx, "ws", "ctx_pool")
	first := trace.finish(&OpenAIForwardResult{})
	require.Equal(t, newUsageTransportDetails("http", "ws", "http_ws_bridge", ""), first.TransportDetails)
	observeUsageTransportFallback(ctx, "ws_failure")
	observeUsageTransport(ctx, "http", "")
	last := trace.finish(&OpenAIForwardResult{})
	require.Equal(t, newUsageTransportDetails("http", "http", "", "ws_failure"), last.TransportDetails)
	observeUsageTransport(ctx, "http", "")
	require.Equal(t, "ws_failure", trace.finish(&OpenAIForwardResult{}).TransportDetails.FallbackReason, "HTTP retries must preserve an explicit fallback reason")
	require.Equal(t, "ws", first.TransportDetails.Upstream)
	_, fresh := withUsageTransportTrace(context.Background(), c)
	require.Nil(t, fresh.finish(&OpenAIForwardResult{}).TransportDetails)
}

func nativeWSTestServiceAccount() (*OpenAIGatewayService, *Account) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Extra: map[string]any{OpenAINativeWSOnlyExtraKey: true, "openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough}}
	return &OpenAIGatewayService{cfg: cfg}, a
}

func TestNativeWSOnlyRuntimeAndConnectionLatch(t *testing.T) {
	s, a := nativeWSTestServiceAccount()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)
	require.NoError(t, s.RequireOpenAINativeWSIngress(c, a))
	other := *a
	other.Extra = map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeHTTPBridge}
	require.Error(t, s.RequireOpenAINativeWSIngress(c, &other))
	require.False(t, other.IsOpenAINativeWSOnly())
	other.Extra = map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool}
	require.NoError(t, s.RequireOpenAINativeWSIngress(c, &other))
	require.True(t, isOpenAINativeWSRequired(c, &other))
	s.cfg.Gateway.OpenAIWS.ForceHTTP = true
	require.Error(t, s.RequireOpenAINativeWSIngress(c, &other))
}

func TestNativeWSOnlyRejectsHTTPAndDisabledCapabilities(t *testing.T) {
	s, a := nativeWSTestServiceAccount()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	_, err := s.Forward(context.Background(), c, a, []byte(`{}`))
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
	s.cfg.Gateway.OpenAIWS.APIKeyEnabled = false
	require.Error(t, s.validateOpenAINativeWSOnly(a))
	s.cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = false
	require.Error(t, s.validateOpenAINativeWSOnly(a))
	require.False(t, s.isOpenAIAccountTransportCompatible(a, OpenAIUpstreamTransportAny))
}

func TestNativeWSOnlyPresetValidationAndLargePayload(t *testing.T) {
	s, a := nativeWSTestServiceAccount()
	s.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	require.NoError(t, ValidateOpenAINativeWSPresetExtra(a.Platform, a.Type, a.Extra))
	require.False(t, s.shouldBridgeOpenAIWSPassthroughFirstMessage(a, []byte(`{"type":"response.create","model":"gpt-5","input":[]}`)))
	require.False(t, s.shouldBridgeOpenAIWSHTTP(a, 100000, ""))
	require.Error(t, ValidateOpenAINativeWSPresetExtra(PlatformKimi, AccountTypeOAuth, a.Extra))
	a.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeHTTPBridge
	require.Error(t, ValidateOpenAINativeWSPresetExtra(a.Platform, a.Type, a.Extra))
}

func TestNativeWSOnlyRejectsPluginAndNonOpenAIUpstreams(t *testing.T) {
	s, a := nativeWSTestServiceAccount()
	a.Type = AccountTypeOAuth
	a.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	s.pluginManager = &PluginManager{}
	s.pluginManager.route.Store(&pluginRoute{rolloutPercent: 100})
	require.ErrorContains(t, s.validateOpenAINativeWSOnly(a), "plugin")
	s.pluginManager = nil
	a.Platform = PlatformGrok
	require.Error(t, s.validateOpenAINativeWSOnly(a))
	a.Platform = PlatformKimi
	require.Error(t, s.validateOpenAINativeWSOnly(a))
}

func TestNativeWSOnlyLatchedPolicyReachesHTTPFinalDefense(t *testing.T) {
	s, a := nativeWSTestServiceAccount()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)
	require.NoError(t, s.RequireOpenAINativeWSIngress(c, a))
	ordinary := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool}}
	ctx, _ := withUsageTransportTrace(context.Background(), c)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/responses", nil)
	_, err := s.doOpenAIUpstream(req, "", ordinary)
	require.ErrorContains(t, err, "native WS-only")
	require.False(t, ordinary.IsOpenAINativeWSOnly())
}
