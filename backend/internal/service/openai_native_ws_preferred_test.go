package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNativeWSPreferredPresetValidation(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			extra := map[string]any{OpenAINativeWSPreferredExtraKey: true}
			require.Error(t, ValidateOpenAINativeWSPresetExtra(PlatformOpenAI, accountType, extra), "the API must explicitly select the pool instead of inheriting a deployment default")
			modeKey := "openai_oauth_responses_websockets_v2_mode"
			if accountType == AccountTypeAPIKey {
				modeKey = "openai_apikey_responses_websockets_v2_mode"
			}
			extra[modeKey] = OpenAIWSIngressModeCtxPool
			require.NoError(t, ValidateOpenAINativeWSPresetExtra(PlatformOpenAI, accountType, extra))
			require.Error(t, ValidateOpenAINativeWSPresetExtra(PlatformKimi, accountType, extra))
		})
	}
	for _, extra := range []map[string]any{
		{OpenAINativeWSPreferredExtraKey: "true"},
		{OpenAINativeWSOnlyExtraKey: 1},
		{OpenAINativeWSPreferredExtraKey: true, OpenAINativeWSOnlyExtraKey: true},
		{OpenAINativeWSPreferredExtraKey: true, "openai_ws_force_http": true},
		{OpenAINativeWSPreferredExtraKey: true, "openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeOff},
		{OpenAINativeWSPreferredExtraKey: true, "openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeHTTPBridge},
		{OpenAINativeWSPreferredExtraKey: true, "openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough},
	} {
		require.Error(t, ValidateOpenAINativeWSPresetExtra(PlatformOpenAI, AccountTypeAPIKey, extra), "%v", extra)
	}
	require.NoError(t, ValidateOpenAINativeWSPresetExtra(PlatformOpenAI, AccountTypeAPIKey, nil))
	require.NoError(t, ValidateOpenAINativeWSPresetExtra(PlatformOpenAI, AccountTypeAPIKey, map[string]any{OpenAINativeWSPreferredExtraKey: false}))
}

func TestNativeWSPreferredExplicitPoolOverridesDeploymentDefault(t *testing.T) {
	for _, defaultMode := range []string{OpenAIWSIngressModeOff, OpenAIWSIngressModePassthrough, OpenAIWSIngressModeHTTPBridge} {
		t.Run(defaultMode, func(t *testing.T) {
			svc, account := nativeWSTestServiceAccount()
			delete(account.Extra, OpenAINativeWSOnlyExtraKey)
			account.Extra[OpenAINativeWSPreferredExtraKey] = true
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
			svc.cfg.Gateway.OpenAIWS.IngressModeDefault = defaultMode
			require.NoError(t, ValidateOpenAINativeWSPresetExtra(account.Platform, account.Type, account.Extra))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			SetOpenAIClientTransport(c, OpenAIClientTransportWS)
			require.NoError(t, svc.RequireOpenAINativeWSIngress(c, account))
		})
	}
}

func TestNativeWSPreferredHTTPStaysHTTPDespiteGlobalBridgeAndGroupFast(t *testing.T) {
	for _, scope := range []string{OpenAIFastBridgeScopeAll, OpenAIFastBridgeScopeFastOnly} {
		for _, tier := range []string{"", OpenAIFastTierPriority, OpenAIFastTierUltrafast} {
			for _, messages := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/messages=%v", scope, tier, messages), func(t *testing.T) {
					svc, upstream, dialer := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), newAnthropicMessagesBridgeCaptureConn(t))
					useAnthropicMessagesBridgeSSEUpstream(upstream)
					setHTTPWSBridgeScope(t, svc, scope)
					account := fastWSBridgeTestAccount(true, false)
					account.Extra[OpenAINativeWSPreferredExtraKey] = true
					account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
					var result *OpenAIForwardResult
					var err error
					if messages {
						body := anthropicMessagesBridgeTestBody(false)
						c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
						result, err = svc.ForwardAsAnthropic(fastWSBridgeTestContext(tier), c, account, body, "", "gpt-6.1-sol")
					} else {
						c, _ := newFastWSBridgeTestGinContext(t)
						result, err = svc.Forward(fastWSBridgeTestContext(tier), c, account, []byte(fastWSBridgeTestBody))
					}
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Zero(t, dialer.DialCount(), "HTTP fallback must not reconnect through WS")
					require.NotNil(t, upstream.lastReq)
					require.False(t, result.OpenAIWSMode)
					require.Equal(t, newUsageTransportDetails("http", "http", "", ""), result.TransportDetails, "a new HTTP request must not invent a WS failure reason")
					require.True(t, account.IsOpenAINativeWSPreferred())
					require.False(t, account.IsOpenAINativeWSOnly())
					require.True(t, svc.openAIFastBridgeEnabled(context.Background()), "account preset must not change the global switch")
				})
			}
		}
	}
}

func TestNativeWSPreferredHTTPPolicyStaysWithinRequest(t *testing.T) {
	svc, _, _ := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), newAnthropicMessagesBridgeCaptureConn(t))
	setHTTPWSBridgeScope(t, svc, OpenAIFastBridgeScopeAll)
	preferred := fastWSBridgeTestAccount(true, false)
	preferred.Extra[OpenAINativeWSPreferredExtraKey] = true
	ordinary := fastWSBridgeTestAccount(true, false)
	c, _ := newFastWSBridgeTestGinContext(t)
	require.NoError(t, svc.requireNativeWSClient(c, preferred, "responses"))
	require.NoError(t, svc.requireNativeWSClient(c, ordinary, "responses"))
	ctx, _ := withUsageTransportTrace(context.Background(), c)
	base := svc.getOpenAIWSProtocolResolver().Resolve(ordinary)
	_, bridged := svc.resolveOpenAIFastWSBridgeDecision(ctx, c, ordinary, []byte(fastWSBridgeTestBody), base)
	require.False(t, bridged)
	_, bridged = svc.resolveAnthropicMessagesWSBridge(ctx, ordinary, "gpt-6.1-sol", []byte(`{"model":"gpt-6.1-sol"}`))
	require.False(t, bridged)
	require.False(t, ordinary.IsOpenAINativeWSPreferred())

	fresh, _ := newFastWSBridgeTestGinContext(t)
	_, bridged = svc.resolveOpenAIFastWSBridgeDecision(context.Background(), fresh, ordinary, []byte(fastWSBridgeTestBody), base)
	require.True(t, bridged, "an independent request must retain its normal routing")
	_, bridged = svc.resolveAnthropicMessagesWSBridge(context.Background(), ordinary, "gpt-6.1-sol", []byte(`{"model":"gpt-6.1-sol"}`))
	require.True(t, bridged)
}

func TestNativeWSPreferredWSRequiresNativePoolAndStrictStillWins(t *testing.T) {
	svc, account := nativeWSTestServiceAccount()
	delete(account.Extra, OpenAINativeWSOnlyExtraKey)
	account.Extra[OpenAINativeWSPreferredExtraKey] = true
	account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)
	require.NoError(t, svc.RequireOpenAINativeWSIngress(c, account))
	require.True(t, isOpenAINativeWSRequired(c, account), "WS ingress must not silently bridge big payloads or plugins to HTTP")
	account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
	require.ErrorContains(t, svc.RequireOpenAINativeWSIngress(c, account), "ctx_pool")
	account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	svc.cfg.Gateway.OpenAIWS.ForceHTTP = true
	require.Error(t, svc.RequireOpenAINativeWSIngress(c, account), "global disable must not be overridden")

	fresh, _ := newFastWSBridgeTestGinContext(t)
	require.NoError(t, svc.requireNativeWSClient(fresh, account, "responses"), "a separate HTTP fallback remains allowed")
	require.False(t, isOpenAINativeWSRequired(fresh, account))
	require.True(t, svc.isOpenAIAccountTransportCompatible(account, OpenAIUpstreamTransportAny))
	account.Extra[OpenAINativeWSOnlyExtraKey] = true // Defend against imported invalid settings.
	require.Error(t, svc.requireNativeWSClient(fresh, account, "responses"))
	require.Equal(t, http.StatusBadRequest, fresh.Writer.Status())
	require.False(t, svc.isOpenAIAccountTransportCompatible(account, OpenAIUpstreamTransportAny))
}

func TestNativeWSPreferredProxyKeepsLargePayloadOnNativeWS(t *testing.T) {
	svc, account, upstream := newIngressControlNativeService()
	defer svc.openaiWSPool.Close()
	account.Extra[OpenAINativeWSPreferredExtraKey] = true
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	results := make(chan *OpenAIForwardResult, 1)
	server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, account, func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) { results <- result }}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-5.1","input":"self-contained request above the configured bridge threshold"}`)
	defer func() { _ = client.CloseNow() }()
	require.Equal(t, "response.create", gjson.GetBytes(requirePassthroughUpstreamWrite(t, upstream, time.Second), "type").String())
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_native_preferred","model":"gpt-5.1","usage":{"input_tokens":2,"output_tokens":1}}}`)
	terminal, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(terminal, "type").String())
	select {
	case result := <-results:
		require.NotNil(t, result)
		require.Equal(t, newUsageTransportDetails("ws", "ws", OpenAIWSIngressModeCtxPool, ""), result.TransportDetails)
	case <-time.After(time.Second):
		t.Fatal("native turn did not complete")
	}
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-serverErr:
		if err != nil {
			require.True(t, isOpenAIWSClientDisconnectError(err), "%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native proxy did not close")
	}
	httpUpstream, ok := svc.httpUpstream.(*httpUpstreamRecorder)
	require.True(t, ok)
	require.Empty(t, httpUpstream.requests)
}

func TestNativeWSPreferredProxyFailureDoesNotInternallyReplayHTTP(t *testing.T) {
	for _, failure := range []string{"global_force_http", "global_disabled", "v2_disabled", "router_disabled", "account_type_disabled", "plugin", "dial_failed"} {
		t.Run(failure, func(t *testing.T) {
			svc, account, upstream := newIngressControlNativeService()
			defer svc.openaiWSPool.Close()
			account.Extra[OpenAINativeWSPreferredExtraKey] = true
			switch failure {
			case "global_force_http":
				svc.cfg.Gateway.OpenAIWS.ForceHTTP = true
			case "global_disabled":
				svc.cfg.Gateway.OpenAIWS.Enabled = false
			case "v2_disabled":
				svc.cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = false
			case "router_disabled":
				svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = false
			case "account_type_disabled":
				svc.cfg.Gateway.OpenAIWS.APIKeyEnabled = false
			case "plugin":
				account.Type = AccountTypeOAuth
				account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
				svc.pluginManager = &PluginManager{}
				svc.pluginManager.route.Store(&pluginRoute{rolloutPercent: 100})
			case "dial_failed":
				svc.openaiWSPool.setClientDialerForTest(&failingFastWSBridgeDialer{})
			}
			server, serverErr := startPassthroughLifecycleServerWithHooks(t, context.Background(), svc, account, nil)
			defer server.Close()
			client := dialPassthroughLifecycleClient(t, server)
			defer func() { _ = client.CloseNow() }()
			_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
			require.Error(t, err)
			select {
			case err := <-serverErr:
				require.Error(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("failed native proxy did not finish")
			}
			httpUpstream, ok := svc.httpUpstream.(*httpUpstreamRecorder)
			require.True(t, ok)
			require.Empty(t, httpUpstream.requests, "HTTP retry must be a separate client request")
			select {
			case payload := <-upstream.writes:
				t.Fatalf("failed connection must not send input: %s", payload)
			default:
			}
		})
	}
}
