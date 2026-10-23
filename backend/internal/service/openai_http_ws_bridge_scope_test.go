package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func setHTTPWSBridgeScope(t *testing.T, svc *OpenAIGatewayService, scope string) {
	t.Helper()
	require.NoError(t, svc.settingService.SetOpenAIFastBridgeSettings(context.Background(), &OpenAIFastBridgeSettings{Enabled: true, Scope: scope}))
}

func TestHTTPWSBridgeScopeStandardRequests(t *testing.T) {
	for _, scope := range []string{OpenAIFastBridgeScopeAll, OpenAIFastBridgeScopeFastOnly} {
		for _, messages := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, explicitDefault := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/messages=%v/stream=%v/default=%v", scope, messages, stream, explicitDefault), func(t *testing.T) {
						conn := newAnthropicMessagesBridgeCaptureConn(t)
						svc, upstream, dialer := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), conn)
						useAnthropicMessagesBridgeSSEUpstream(upstream)
						setHTTPWSBridgeScope(t, svc, scope)
						ctx := context.Background()
						if explicitDefault {
							ctx = context.WithValue(ctx, ctxkey.Group, &Group{ID: 7, Platform: PlatformComposite, Status: StatusActive, Hydrated: true, ForcedServiceTier: OpenAIFastTierDefault})
						}
						account := fastWSBridgeTestAccount(true, false)
						var result *OpenAIForwardResult
						var err error
						if messages {
							body := anthropicMessagesBridgeTestBody(stream)
							c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
							result, err = svc.ForwardAsAnthropic(ctx, c, account, body, "", "gpt-6.1-sol")
							if stream {
								require.Contains(t, rec.Body.String(), "event: message_start")
							} else {
								require.Equal(t, "message", gjson.Get(rec.Body.String(), "type").String())
							}
						} else {
							c, _ := newFastWSBridgeTestGinContext(t)
							body := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","stream":%t,"store":false,"instructions":"test","input":[{"role":"user","content":"hi"}]}`, stream))
							result, err = svc.Forward(ctx, c, account, body)
						}
						require.NoError(t, err)
						require.NotNil(t, result)
						wantWS := scope == OpenAIFastBridgeScopeAll
						require.Equal(t, wantWS, result.OpenAIWSMode)
						if wantWS {
							require.Equal(t, 1, dialer.DialCount())
							require.Nil(t, upstream.lastReq)
							tier := gjson.Get(requestToJSONString(conn.lastWrite), "service_tier")
							if explicitDefault {
								require.Equal(t, OpenAIFastTierDefault, tier.String())
							} else {
								require.False(t, tier.Exists(), "scope must not inject Fast into a standard request")
							}
						} else {
							require.Zero(t, dialer.DialCount())
							require.NotNil(t, upstream.lastReq)
						}
						if explicitDefault {
							require.NotNil(t, result.ServiceTier)
							require.Equal(t, OpenAIFastTierDefault, *result.ServiceTier)
						} else {
							require.Nil(t, result.ServiceTier)
						}
					})
				}
			}
		}
	}
}

func TestHTTPWSBridgeLegacyEnabledSettingsDefaultToAll(t *testing.T) {
	for _, messages := range []bool{false, true} {
		t.Run(fmt.Sprintf("messages=%v", messages), func(t *testing.T) {
			svc, upstream, dialer := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), newAnthropicMessagesBridgeCaptureConn(t))
			svc.settingService = NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{
				SettingKeyOpenAIFastBridgeSettings: `{"enabled":true}`,
			}}, nil)
			account := fastWSBridgeTestAccount(true, false)
			var result *OpenAIForwardResult
			var err error
			if messages {
				body := anthropicMessagesBridgeTestBody(false)
				c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
				result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-6.1-sol")
			} else {
				c, _ := newFastWSBridgeTestGinContext(t)
				result, err = svc.Forward(context.Background(), c, account, []byte(fastWSBridgeTestBody))
			}
			require.NoError(t, err)
			require.True(t, result.OpenAIWSMode)
			require.Equal(t, 1, dialer.DialCount())
			require.Nil(t, upstream.lastReq)
			require.Nil(t, result.ServiceTier)
		})
	}
}

func TestHTTPWSBridgeStandardFallsBackWithoutUpgradingTier(t *testing.T) {
	for _, messages := range []bool{false, true} {
		t.Run(fmt.Sprintf("messages=%v", messages), func(t *testing.T) {
			cfg := newOpenAIWSV2TestConfig()
			svc, upstream, _ := newFastWSBridgeTestService(t, cfg, newAnthropicMessagesBridgeCaptureConn(t))
			useAnthropicMessagesBridgeSSEUpstream(upstream)
			setHTTPWSBridgeScope(t, svc, OpenAIFastBridgeScopeAll)
			useFastWSBridgeTestDialer(svc, cfg, &failingFastWSBridgeDialer{})
			account := fastWSBridgeTestAccount(true, false)
			var result *OpenAIForwardResult
			var err error
			if messages {
				body := anthropicMessagesBridgeTestBody(false)
				c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
				result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-6.1-sol")
			} else {
				c, _ := newFastWSBridgeTestGinContext(t)
				result, err = svc.Forward(context.Background(), c, account, []byte(fastWSBridgeTestBody))
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.OpenAIWSMode)
			require.NotNil(t, upstream.lastReq)
			require.False(t, gjson.GetBytes(upstream.lastBody, "service_tier").Exists())
			require.Nil(t, result.ServiceTier)
		})
	}
}

func TestHTTPWSBridgeAllScopePreservesHardGates(t *testing.T) {
	for _, name := range []string{"disabled", "global_force_http", "global_ws_off", "oauth_off", "v2_off", "account_force_http", "apikey", "passthrough", "compact"} {
		for _, messages := range []bool{false, true} {
			if messages && name == "compact" {
				continue
			}
			t.Run(fmt.Sprintf("%s/messages=%v", name, messages), func(t *testing.T) {
				cfg := newOpenAIWSV2TestConfig()
				account := fastWSBridgeTestAccount(true, false)
				switch name {
				case "global_force_http":
					cfg.Gateway.OpenAIWS.ForceHTTP = true
				case "global_ws_off":
					cfg.Gateway.OpenAIWS.Enabled = false
				case "oauth_off":
					cfg.Gateway.OpenAIWS.OAuthEnabled = false
				case "v2_off":
					cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = false
				case "account_force_http":
					account.Extra["openai_ws_force_http"] = true
				case "apikey":
					account.Type = AccountTypeAPIKey
					account.Credentials = map[string]any{"api_key": "sk-test"}
				case "passthrough":
					account.Extra["openai_passthrough"] = true
				}
				svc, _, _ := newFastWSBridgeTestService(t, cfg, newAnthropicMessagesBridgeCaptureConn(t))
				require.NoError(t, svc.settingService.SetOpenAIFastBridgeSettings(context.Background(), &OpenAIFastBridgeSettings{Enabled: name != "disabled", Scope: OpenAIFastBridgeScopeAll}))
				var bridged bool
				if messages {
					_, bridged = svc.resolveAnthropicMessagesWSBridge(context.Background(), account, "gpt-6.1-sol", []byte(`{"model":"gpt-6.1-sol","service_tier":"default"}`))
				} else {
					c, _ := newFastWSBridgeTestGinContext(t)
					if name == "compact" {
						c.Request.URL.Path = "/v1/responses/compact"
					}
					base := svc.getOpenAIWSProtocolResolver().Resolve(account)
					_, bridged = svc.resolveOpenAIFastWSBridgeDecision(context.Background(), c, account, []byte(fastWSBridgeTestBody), base)
				}
				require.False(t, bridged)
			})
		}
	}
}

func TestHTTPWSBridgeAllScopeDoesNotOverrideStandardAccountOptOut(t *testing.T) {
	for _, messages := range []bool{false, true} {
		for _, forcedDefault := range []bool{false, true} {
			t.Run(fmt.Sprintf("messages=%v/forcedDefault=%v", messages, forcedDefault), func(t *testing.T) {
				svc, upstream, dialer := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), newAnthropicMessagesBridgeCaptureConn(t))
				useAnthropicMessagesBridgeSSEUpstream(upstream)
				setHTTPWSBridgeScope(t, svc, OpenAIFastBridgeScopeAll)
				ctx := context.Background()
				if forcedDefault {
					ctx = fastWSBridgeTestContext(OpenAIFastTierDefault)
				}
				account := fastWSBridgeTestAccount(false, false)
				var result *OpenAIForwardResult
				var err error
				if messages {
					body := anthropicMessagesBridgeTestBody(false)
					c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
					result, err = svc.ForwardAsAnthropic(ctx, c, account, body, "", "gpt-6.1-sol")
				} else {
					c, _ := newFastWSBridgeTestGinContext(t)
					result, err = svc.Forward(ctx, c, account, []byte(fastWSBridgeTestBody))
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.False(t, result.OpenAIWSMode)
				require.Zero(t, dialer.DialCount())
				require.NotNil(t, upstream.lastReq)
			})
		}
	}
}
