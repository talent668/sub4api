package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newFastWSBridgeTestService(t *testing.T, cfg *config.Config, captureConn *openAIWSCaptureConn) (*OpenAIGatewayService, *httpUpstreamRecorder, *openAIWSCaptureDialer) {
	t.Helper()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	// Most tests exercise the bridge; the disabled-switch case sets it back to false.
	cfg.Gateway.OpenAIWS.FastBridgeEnabled = true
	pool := newOpenAIWSConnPool(cfg)
	dialer := &openAIWSCaptureDialer{conn: captureConn}
	pool.setClientDialerForTest(dialer)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_http_fast_bridge","usage":{"input_tokens":1,"output_tokens":2}}`,
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     upstream,
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
		settingService: NewSettingService(
			&openAIFastPolicyRepoStub{values: map[string]string{}},
			&config.Config{},
		),
	}
	return svc, upstream, dialer
}

func fastWSBridgeTestAccount(wsEnabled, passthrough bool) *Account {
	extra := map[string]any{}
	if wsEnabled {
		extra["responses_websockets_v2_enabled"] = true
	}
	if passthrough {
		extra["openai_passthrough"] = true
	}
	return &Account{
		ID:          501,
		Name:        "openai-oauth-fast",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token-fast",
			"chatgpt_account_id": "chatgpt-acc-fast",
		},
		Extra: extra,
	}
}

func fastWSBridgeTestContext(tier string) context.Context {
	ctx := context.Background()
	if tier == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.Group, &Group{
		ID:                7,
		Platform:          PlatformOpenAI,
		Status:            StatusActive,
		Hydrated:          true,
		ForcedServiceTier: tier,
	})
}

func newFastWSBridgeTestCaptureConn(t *testing.T) *openAIWSCaptureConn {
	t.Helper()
	return &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.completed","response":{"id":"resp_fast_1","model":"gpt-6.1-sol","usage":{"input_tokens":3,"output_tokens":2}}}`),
	}}
}

func newFastWSBridgeTestGinContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.163.0-alpha.11.1")
	c.Request.Header.Set("originator", "codex_cli_rs")
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c, rec
}

const fastWSBridgeTestBody = `{"model":"gpt-6.1-sol","stream":true,"store":false,"instructions":"test","input":[{"type":"input_text","text":"hello"}]}`

// TestOpenAIGatewayService_Forward_HTTPIngressBridgesFastTierToWS is the core
// regression for Fast/Ultra Fast: HTTP/SSE downstream + group-forced priority
// must dial the upstream Responses WebSocket, because the ChatGPT Codex backend
// only schedules priority on that transport.
func TestOpenAIGatewayService_Forward_HTTPIngressBridgesFastTierToWS(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	captureConn := newFastWSBridgeTestCaptureConn(t)
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, captureConn)
	c, rec := newFastWSBridgeTestGinContext(t)

	body := []byte(fastWSBridgeTestBody)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode, "HTTP ingress with forced priority must use the WS upstream")
	require.Nil(t, upstream.lastReq, "fast bridge must not fall back to HTTP when WS succeeds")
	require.NotNil(t, captureConn.lastWrite)
	require.Equal(t, OpenAIFastTierPriority, gjson.Get(requestToJSONString(captureConn.lastWrite), "service_tier").String())
	// The downstream is still HTTP/SSE: the WS upstream events must be re-encoded
	// as SSE frames for the HTTP client.
	require.Contains(t, rec.Body.String(), "data: ")
	require.Contains(t, rec.Body.String(), "response.completed")

	reason, ok := c.Get("openai_ws_transport_reason")
	require.True(t, ok)
	require.Equal(t, "fast_tier_http_bridge", reason)
}

// TestOpenAIGatewayService_Forward_HTTPIngressForcesWSForGroupFastWhenAccountWSDisabled
// verifies the group-forced tier is an explicit admin intent: it may use WS even
// when the account-level WS switch is off, as long as global WS is available.
func TestOpenAIGatewayService_Forward_HTTPIngressForcesWSForGroupFastWhenAccountWSDisabled(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	captureConn := newFastWSBridgeTestCaptureConn(t)
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, captureConn)
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(fastWSBridgeTestBody)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(false, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode)
	require.Nil(t, upstream.lastReq)
	require.Equal(t, OpenAIFastTierPriority, gjson.Get(requestToJSONString(captureConn.lastWrite), "service_tier").String())

	reason, ok := c.Get("openai_ws_transport_reason")
	require.True(t, ok)
	require.Equal(t, "fast_tier_group_forced_ws", reason)
}

// TestOpenAIGatewayService_Forward_HTTPIngressKeepsHTTPForClientFastWhenAccountWSDisabled
// keeps the account-level WS opt-out authoritative for client-sent tiers.
func TestOpenAIGatewayService_Forward_HTTPIngressKeepsHTTPForClientFastWhenAccountWSDisabled(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	captureConn := newFastWSBridgeTestCaptureConn(t)
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, captureConn)
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"service_tier":"priority","instructions":"test","input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(context.Background(), c, fastWSBridgeTestAccount(false, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode, "account-level WS opt-out must keep client-sent fast on HTTP")
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, OpenAIFastTierPriority, gjson.GetBytes(upstream.lastBody, "service_tier").String())
}

// TestOpenAIGatewayService_Forward_HTTPIngressKeepsHTTPForForcedDefaultTier
// makes sure the bridge only activates for priority/ultrafast, not for an
// explicit standard/default group tier.
func TestOpenAIGatewayService_Forward_HTTPIngressKeepsHTTPForForcedDefaultTier(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	captureConn := newFastWSBridgeTestCaptureConn(t)
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, captureConn)
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"instructions":"test","input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierDefault), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, OpenAIFastTierDefault, gjson.GetBytes(upstream.lastBody, "service_tier").String())
}

// TestOpenAIGatewayService_Forward_HTTPIngressPassthroughAccountBridgesFastTier
// covers auto-passthrough accounts: the fast bridge skips the passthrough
// dispatcher so the shared WS branch (and its fast policy) handles the turn.
func TestOpenAIGatewayService_Forward_HTTPIngressPassthroughAccountBridgesFastTier(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	captureConn := newFastWSBridgeTestCaptureConn(t)
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, captureConn)
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(fastWSBridgeTestBody)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, true), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode)
	require.Nil(t, upstream.lastReq)
	require.Equal(t, OpenAIFastTierPriority, gjson.Get(requestToJSONString(captureConn.lastWrite), "service_tier").String())
}

// TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallsBackToHTTP verifies
// the WS bridge is best-effort: when the upstream WS cannot be dialed, the turn
// is served over HTTP so the request still completes (at standard tier).
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallsBackToHTTP(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&failingFastWSBridgeDialer{})
	svc.openaiWSPool = pool
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"instructions":"test","input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.NotNil(t, upstream.lastReq, "WS dial failure must fall back to HTTP")
	require.Equal(t, OpenAIFastTierPriority, gjson.GetBytes(upstream.lastBody, "service_tier").String())

	reason, ok := c.Get("openai_ws_transport_reason")
	require.True(t, ok)
	require.Equal(t, "fast_bridge_fallback_http", reason)
}

// TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeDisabledStaysHTTP covers
// the opt-in switch: with fast_bridge_enabled=false a forced priority request
// keeps the legacy HTTP forwarding.
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeDisabledStaysHTTP(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	svc.cfg.Gateway.OpenAIWS.FastBridgeEnabled = false
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"instructions":"test","input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, OpenAIFastTierPriority, gjson.GetBytes(upstream.lastBody, "service_tier").String())
	require.Equal(t, 0, dialer.DialCount(), "disabled bridge must not dial upstream WS")
}

type failingFastWSBridgeDialer struct{}

func (d *failingFastWSBridgeDialer) Dial(
	ctx context.Context,
	wsURL string,
	headers http.Header,
	proxyURL string,
) (openAIWSClientConn, int, http.Header, error) {
	_, _, _, _ = ctx, wsURL, headers, proxyURL
	return nil, 0, nil, errors.New("fast bridge dial refused")
}
