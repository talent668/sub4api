package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
			&openAIFastPolicyRepoStub{values: map[string]string{
				SettingKeyOpenAIFastBridgeSettings: `{"enabled":true,"scope":"fast_only"}`,
			}},
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

// TestOpenAIGatewayService_Forward_HTTPIngressPassthroughAccountNotBridged
// keeps auto-passthrough accounts out of the fast bridge: the shared WS branch
// would bypass the passthrough contract (instructions gate, Codex transform,
// namespace restore), so they stay on the passthrough HTTP path.
func TestOpenAIGatewayService_Forward_HTTPIngressPassthroughAccountNotBridged(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	useFastWSBridgeTestSSEUpstream(upstream)
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"instructions":"test","input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, true), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, 0, dialer.DialCount(), "passthrough accounts must not dial the upstream WS")
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, OpenAIFastTierPriority, gjson.GetBytes(upstream.lastBody, "service_tier").String())
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
// the web-only opt-in switch: with the Admin Settings toggle off a forced
// priority request keeps the legacy HTTP forwarding.
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeDisabledStaysHTTP(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	svc.settingService.settingRepo = &openAIFastPolicyRepoStub{values: map[string]string{
		SettingKeyOpenAIFastBridgeSettings: `{"enabled":false}`,
	}}
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

func setFastWSBridgeTestPolicy(t *testing.T, svc *OpenAIGatewayService, rules ...OpenAIFastPolicyRule) {
	t.Helper()
	repo, ok := svc.settingService.settingRepo.(*openAIFastPolicyRepoStub)
	require.True(t, ok)
	raw, err := json.Marshal(&OpenAIFastPolicySettings{Rules: rules})
	require.NoError(t, err)
	repo.values[SettingKeyOpenAIFastPolicySettings] = string(raw)
}

// sequenceFastWSBridgeDialer hands out the queued connections in order and
// refuses every dial after the queue is drained.
type sequenceFastWSBridgeDialer struct {
	mu        sync.Mutex
	conns     []openAIWSClientConn
	dialCount int
}

func (d *sequenceFastWSBridgeDialer) Dial(
	ctx context.Context,
	wsURL string,
	headers http.Header,
	proxyURL string,
) (openAIWSClientConn, int, http.Header, error) {
	_, _, _, _ = ctx, wsURL, headers, proxyURL
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dialCount++
	if len(d.conns) == 0 {
		return nil, 0, nil, errors.New("fast bridge dial refused")
	}
	conn := d.conns[0]
	d.conns = d.conns[1:]
	return conn, 0, nil, nil
}

// useFastWSBridgeTestSSEUpstream makes the HTTP upstream answer with SSE: the
// OAuth passthrough path forces stream=true and rejects a JSON body.
func useFastWSBridgeTestSSEUpstream(upstream *httpUpstreamRecorder) {
	upstream.resp = &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.completed","response":{"id":"resp_passthrough_fast","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"))),
	}
}

func useFastWSBridgeTestDialer(svc *OpenAIGatewayService, cfg *config.Config, dialer openAIWSClientDialer) {
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	svc.openaiWSPool = pool
}

// TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFirstOutputTimeout keeps
// gateway.openai_first_output_timeout_seconds effective after an HTTP stream is
// bridged to the upstream WS: no semantic output before the deadline must end
// the attempt with the same first_output_timeout failover as the HTTP path.
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFirstOutputTimeout(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
	captureConn := &openAIWSCaptureConn{
		readDelays: []time.Duration{3 * time.Second},
		events: [][]byte{
			[]byte(`{"type":"response.output_text.delta","delta":"late"}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_fast_late","model":"gpt-6.1-sol","usage":{"input_tokens":3,"output_tokens":2}}}`),
		},
	}
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, captureConn)
	c, rec := newFastWSBridgeTestGinContext(t)

	startedAt := time.Now()
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, false), []byte(fastWSBridgeTestBody))
	elapsed := time.Since(startedAt)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Contains(t, string(failoverErr.ResponseBody), "first_output_timeout")
	require.Less(t, elapsed, 2500*time.Millisecond, "first output deadline must not wait for the 900s WS read timeout")
	require.Nil(t, upstream.lastReq, "deadline already passed: HTTP fallback would time out immediately")
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

// TestOpenAIGatewayService_Forward_HTTPIngressPassthroughFlattenNamespacesNotBridged
// keeps passthrough accounts with namespace flattening on the passthrough HTTP
// path: the shared WS branch never restores flattened tool names.
func TestOpenAIGatewayService_Forward_HTTPIngressPassthroughFlattenNamespacesNotBridged(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	useFastWSBridgeTestSSEUpstream(upstream)
	c, _ := newFastWSBridgeTestGinContext(t)
	account := fastWSBridgeTestAccount(true, true)
	account.Extra["openai_responses_flatten_namespaces"] = true

	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, account, []byte(codexNamespaceRequestBody))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, 0, dialer.DialCount())
	require.NotNil(t, upstream.lastReq)
	// 透传 HTTP 出口自带回程还原，摊平名可以安全使用。
	require.Contains(t, string(upstream.lastBody), "collaboration__spawn_agent")
	require.NotEmpty(t, openAIResponsesNamespaceNames(c))
}

// TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallbackDropsPrevIDWithToolOutput
// covers the case WS recovery cannot repair (function_call_output keeps
// previous_response_id): the HTTP fallback must still drop it, because the
// OAuth HTTP upstream only accepts previous_response_id on Responses WSv2.
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallbackDropsPrevIDWithToolOutput(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	notFoundConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"error","error":{"code":"previous_response_not_found","type":"invalid_request_error","message":"Previous response with id 'resp_prev_fast' not found."}}`),
	}}
	dialer := &sequenceFastWSBridgeDialer{conns: []openAIWSClientConn{notFoundConn}}
	useFastWSBridgeTestDialer(svc, cfg, dialer)
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"previous_response_id":"resp_prev_fast","instructions":"test","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"},{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.NotNil(t, notFoundConn.lastWrite)
	require.Equal(t, "resp_prev_fast", gjson.Get(requestToJSONString(notFoundConn.lastWrite), "previous_response_id").String())
	require.NotNil(t, upstream.lastReq)
	require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
}

// TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallbackDropsPreviousResponseID
// mirrors the non-WSv2 rule for OAuth accounts: a bridged request that falls
// back to HTTP must not carry previous_response_id.
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallbackDropsPreviousResponseID(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	useFastWSBridgeTestDialer(svc, cfg, &failingFastWSBridgeDialer{})
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"previous_response_id":"resp_prev_fast","instructions":"test","input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.NotNil(t, upstream.lastReq)
	require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
	require.Equal(t, OpenAIFastTierPriority, gjson.GetBytes(upstream.lastBody, "service_tier").String())
}

// TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallbackKeepsWSRecovery
// makes sure the HTTP fallback replays the request the WS retry loop already
// repaired: once WS dropped the rejected encrypted reasoning items, HTTP must
// not resend them.
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallbackKeepsWSRecovery(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	rejectConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"error","error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"The encrypted content could not be verified."}}`),
	}}
	dialer := &sequenceFastWSBridgeDialer{conns: []openAIWSClientConn{rejectConn}}
	useFastWSBridgeTestDialer(svc, cfg, dialer)
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"instructions":"test","input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA_rejected"},{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	// The redial only happens after the WS recovery trimmed the rejected item.
	// lastWrite is a shallow clone that shares the trimmed input items, so it
	// cannot prove what the first attempt sent.
	require.GreaterOrEqual(t, dialer.dialCount, 2, "WS recovery retries once before falling back")
	require.NotNil(t, rejectConn.lastWrite)
	require.NotNil(t, upstream.lastReq)
	require.NotContains(t, string(upstream.lastBody), "gAAA_rejected")
	require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, `input.#(type=="input_text").text`).String())
}

// TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallbackHTTPInvalidEncryptedRetry
// keeps the HTTP invalid_encrypted_content recovery working after a bridged
// request fell back (the decoded request map must not be the WS one).
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeFallbackHTTPInvalidEncryptedRetry(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	useFastWSBridgeTestDialer(svc, cfg, &failingFastWSBridgeDialer{})
	upstream.responses = []*http.Response{
		{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"code":"invalid_encrypted_content","type":"invalid_request_error","message":"The encrypted content could not be verified."}}`,
			)),
		},
	}
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"instructions":"test","input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA_rejected"},{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.Contains(t, string(upstream.bodies[0]), "gAAA_rejected")
	require.NotContains(t, string(upstream.bodies[1]), "gAAA_rejected")
}

// TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeUsesUpstreamModelForPolicy
// requires the bridge pre-check to evaluate the fast policy against the same
// upstream model Forward uses (Codex normalization gpt-5.4-high -> gpt-5.4).
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeUsesUpstreamModelForPolicy(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	captureConn := newFastWSBridgeTestCaptureConn(t)
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, captureConn)
	setFastWSBridgeTestPolicy(t, svc, OpenAIFastPolicyRule{
		ServiceTier:    OpenAIFastTierMissing,
		Action:         OpenAIFastPolicyActionForcePriority,
		Scope:          BetaPolicyScopeAll,
		ModelWhitelist: []string{"gpt-5.4"},
	})
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-5.4-high","stream":false,"store":false,"instructions":"test","input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(context.Background(), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode, "Forward forces priority for gpt-5.4, so the request must be bridged")
	require.Nil(t, upstream.lastReq)
	forwarded := requestToJSONString(captureConn.lastWrite)
	require.Equal(t, "gpt-5.4", gjson.Get(forwarded, "model").String())
	require.Equal(t, OpenAIFastTierPriority, gjson.Get(forwarded, "service_tier").String())
}

// TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeMissingTierForcedOnce
// mirrors Forward: priority injected by missing->force_priority is written as-is
// and is not evaluated again against a priority rule.
func TestOpenAIGatewayService_Forward_HTTPIngressFastBridgeMissingTierForcedOnce(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	captureConn := newFastWSBridgeTestCaptureConn(t)
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, captureConn)
	setFastWSBridgeTestPolicy(t, svc,
		OpenAIFastPolicyRule{ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll},
		OpenAIFastPolicyRule{ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionFilter, Scope: BetaPolicyScopeAll},
	)
	c, _ := newFastWSBridgeTestGinContext(t)

	body := []byte(`{"model":"gpt-6.1-sol","stream":false,"store":false,"instructions":"test","input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(context.Background(), c, fastWSBridgeTestAccount(true, false), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode)
	require.Nil(t, upstream.lastReq)
	require.Equal(t, OpenAIFastTierPriority, gjson.Get(requestToJSONString(captureConn.lastWrite), "service_tier").String())
}

func TestOpenAIGatewayService_EffectiveOpenAIServiceTierForTransportMirrorsForward(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, _, _ := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	setFastWSBridgeTestPolicy(t, svc,
		OpenAIFastPolicyRule{ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll},
		OpenAIFastPolicyRule{ServiceTier: OpenAIFastTierPriority, Action: BetaPolicyActionFilter, Scope: BetaPolicyScopeAll},
	)
	account := fastWSBridgeTestAccount(true, false)

	// Missing tier: forced priority is final (no second evaluation).
	require.Equal(t, OpenAIFastTierPriority, svc.effectiveOpenAIServiceTierForTransport(
		context.Background(), account, "gpt-6.1-sol", []byte(`{"model":"gpt-6.1-sol"}`)))
	// Client priority: the priority rule filters it.
	require.Equal(t, "", svc.effectiveOpenAIServiceTierForTransport(
		context.Background(), account, "gpt-6.1-sol", []byte(`{"model":"gpt-6.1-sol","service_tier":"priority"}`)))
	// Unrecognized client tier: Forward keeps it untouched and never applies
	// the missing-tier force.
	require.Equal(t, "", svc.effectiveOpenAIServiceTierForTransport(
		context.Background(), account, "gpt-6.1-sol", []byte(`{"model":"gpt-6.1-sol","service_tier":"turbo"}`)))
}

type countingFastBridgeSettingRepo struct {
	openAIFastPolicyRepoStub
	bridgeReads atomic.Int32
	bridgeErr   error
}

func (r *countingFastBridgeSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if key == SettingKeyOpenAIFastBridgeSettings {
		r.bridgeReads.Add(1)
		if r.bridgeErr != nil {
			return "", r.bridgeErr
		}
	}
	return r.openAIFastPolicyRepoStub.GetValue(ctx, key)
}

// TestOpenAIGatewayService_FastBridgeDecisionSkipsSwitchReadForIneligibleRequests
// keeps the bridge switch off the hot path for requests that can never be
// bridged (compact, empty model, ...).
func TestOpenAIGatewayService_FastBridgeDecisionSkipsSwitchReadForIneligibleRequests(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, _, _ := newFastWSBridgeTestService(t, cfg, newFastWSBridgeTestCaptureConn(t))
	repo := &countingFastBridgeSettingRepo{openAIFastPolicyRepoStub: openAIFastPolicyRepoStub{values: map[string]string{
		SettingKeyOpenAIFastBridgeSettings: `{"enabled":true}`,
	}}}
	svc.settingService = NewSettingService(repo, &config.Config{})
	account := fastWSBridgeTestAccount(true, false)
	base := svc.getOpenAIWSProtocolResolver().Resolve(account)

	compactCtx, _ := newFastWSBridgeTestGinContext(t)
	compactCtx.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses/compact", nil)
	SetOpenAIClientTransport(compactCtx, OpenAIClientTransportHTTP)
	_, bridged := svc.resolveOpenAIFastWSBridgeDecision(fastWSBridgeTestContext(OpenAIFastTierPriority), compactCtx, account, []byte(fastWSBridgeTestBody), base)
	require.False(t, bridged)

	emptyModelCtx, _ := newFastWSBridgeTestGinContext(t)
	_, bridged = svc.resolveOpenAIFastWSBridgeDecision(fastWSBridgeTestContext(OpenAIFastTierPriority), emptyModelCtx, account, []byte(`{"input":"hi"}`), base)
	require.False(t, bridged)

	require.Equal(t, int32(0), repo.bridgeReads.Load(), "ineligible requests must not read the bridge switch")
}

// TestOpenAIGatewayService_FastBridgeSwitchIsCached keeps the web switch off the
// DB per request while a save in this process takes effect immediately.
func TestOpenAIGatewayService_FastBridgeSwitchIsCached(t *testing.T) {
	repo := &countingFastBridgeSettingRepo{openAIFastPolicyRepoStub: openAIFastPolicyRepoStub{values: map[string]string{
		SettingKeyOpenAIFastBridgeSettings: `{"enabled":true}`,
	}}}
	svc := &OpenAIGatewayService{settingService: NewSettingService(repo, &config.Config{})}
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.True(t, svc.openAIFastBridgeEnabled(ctx))
	}
	require.Equal(t, int32(1), repo.bridgeReads.Load())

	require.NoError(t, svc.settingService.SetOpenAIFastBridgeSettings(ctx, &OpenAIFastBridgeSettings{Enabled: false, Scope: OpenAIFastBridgeScopeAll}))
	require.False(t, svc.openAIFastBridgeEnabled(ctx), "saving the switch must take effect immediately in-process")
	require.NoError(t, svc.settingService.SetOpenAIFastBridgeSettings(ctx, &OpenAIFastBridgeSettings{Enabled: true, Scope: OpenAIFastBridgeScopeAll}))
	require.True(t, svc.openAIFastBridgeEnabled(ctx))
	require.Equal(t, int32(1), repo.bridgeReads.Load())
}

func TestOpenAIGatewayService_FastBridgeSwitchReadErrorMeansDisabled(t *testing.T) {
	repo := &countingFastBridgeSettingRepo{
		openAIFastPolicyRepoStub: openAIFastPolicyRepoStub{values: map[string]string{}},
		bridgeErr:                errors.New("db down"),
	}
	svc := &OpenAIGatewayService{settingService: NewSettingService(repo, &config.Config{})}
	require.False(t, svc.openAIFastBridgeEnabled(context.Background()))
}
