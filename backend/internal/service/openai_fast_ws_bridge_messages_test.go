package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newAnthropicMessagesBridgeTestGinContext(t *testing.T, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.0 (external, cli)")
	return c, rec
}

func anthropicMessagesBridgeTestBody(stream bool) []byte {
	if stream {
		return []byte(`{"model":"gpt-6.1-sol","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	}
	return []byte(`{"model":"gpt-6.1-sol","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
}

func newAnthropicMessagesBridgeCaptureConn(t *testing.T) *openAIWSCaptureConn {
	t.Helper()
	return &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_msg_ws","model":"gpt-6.1-sol","status":"in_progress","output":[]}}`),
		[]byte(`{"type":"response.output_text.delta","delta":"ok"}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_msg_ws","object":"response","model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`),
	}}
}

func useAnthropicMessagesBridgeSSEUpstream(upstream *httpUpstreamRecorder) {
	upstream.resp = &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_msg_http"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_msg_http","model":"gpt-6.1-sol","status":"in_progress","output":[]}}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"ok"}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_msg_http","object":"response","model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"))),
	}
}

// TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeUsesUpstreamWS locks the
// core fix: an Anthropic Messages request served by a ChatGPT OAuth/Codex
// account at a fast tier must be bridged to the upstream Responses WS, otherwise
// the ChatGPT Codex backend silently downgrades priority on HTTP.
func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeUsesUpstreamWS(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	captureConn := newAnthropicMessagesBridgeCaptureConn(t)
	svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, captureConn)
	body := anthropicMessagesBridgeTestBody(true)
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
	account := fastWSBridgeTestAccount(false, false)

	result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, account, body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode, "Messages ingress with forced priority must use the upstream WS")
	require.Equal(t, 1, dialer.DialCount())
	require.Nil(t, upstream.lastReq, "fast bridge must not fall back to HTTP when WS succeeds")
	require.Equal(t, OpenAIFastTierPriority, gjson.Get(requestToJSONString(captureConn.lastWrite), "service_tier").String())

	// Downstream stays Anthropic SSE, re-encoded from the WS events.
	downstream := rec.Body.String()
	require.Contains(t, downstream, "event: ")
	require.Contains(t, downstream, "ok")

	reason, ok := c.Get("openai_ws_transport_reason")
	require.True(t, ok)
	require.Equal(t, "fast_tier_messages_bridge", reason)
}

// TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeFallsBackToHTTP keeps the
// bridge best-effort: a WS dial failure before any downstream byte must complete
// the request over the existing HTTP compat path (at standard scheduling).
func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeFallsBackToHTTP(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, newAnthropicMessagesBridgeCaptureConn(t))
	useAnthropicMessagesBridgeSSEUpstream(upstream)
	useFastWSBridgeTestDialer(svc, cfg, &sequenceFastWSBridgeDialer{})
	body := anthropicMessagesBridgeTestBody(true)
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)

	result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.NotNil(t, upstream.lastReq, "WS dial failure must fall back to HTTP")
	require.Equal(t, OpenAIFastTierPriority, gjson.GetBytes(upstream.lastBody, "service_tier").String())
	require.Contains(t, rec.Body.String(), "event: ")
}

// TestOpenAIGatewayService_ForwardAsAnthropic_NoFastTierKeepsHTTP keeps the
// regular Messages compat path untouched when no fast tier is requested.
func TestOpenAIGatewayService_ForwardAsAnthropic_NoFastTierKeepsHTTP(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, newAnthropicMessagesBridgeCaptureConn(t))
	useAnthropicMessagesBridgeSSEUpstream(upstream)
	body := anthropicMessagesBridgeTestBody(true)
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, 0, dialer.DialCount())
	require.NotNil(t, upstream.lastReq)
	require.Contains(t, rec.Body.String(), "event: ")
}

// TestOpenAIGatewayService_ForwardAsAnthropic_NonStreamBridgesWS covers the
// non-streaming client case: the WS terminal response must still be re-encoded
// as an Anthropic Messages JSON body.
func TestOpenAIGatewayService_ForwardAsAnthropic_NonStreamBridgesWS(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, newAnthropicMessagesBridgeCaptureConn(t))
	body := anthropicMessagesBridgeTestBody(false)
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
	account := fastWSBridgeTestAccount(false, false)

	result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, account, body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode)
	require.Equal(t, 1, dialer.DialCount())
	require.Nil(t, upstream.lastReq)
	require.Equal(t, "message", gjson.Get(rec.Body.String(), "type").String())
	require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
}

// TestOpenAIGatewayService_ForwardAsAnthropic_ClientFastKeepsHTTPWhenAccountWSDisabled
// mirrors the /v1/responses semantics: a client-sent fast tier still respects
// the account-level WS opt-out; only a group-forced tier may override it.
func TestOpenAIGatewayService_ForwardAsAnthropic_ClientFastKeepsHTTPWhenAccountWSDisabled(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, newAnthropicMessagesBridgeCaptureConn(t))
	useAnthropicMessagesBridgeSSEUpstream(upstream)
	body := []byte(`{"model":"gpt-6.1-sol","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
	c.Request.Header.Set("anthropic-beta", "fast-mode-2026-02-01")

	result, err := svc.ForwardAsAnthropic(context.Background(), c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, 0, dialer.DialCount())
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, OpenAIFastTierPriority, gjson.GetBytes(upstream.lastBody, "service_tier").String())
	require.Contains(t, rec.Body.String(), "event: ")
}

// TestOpenAIGatewayService_ForwardAsAnthropic_GlobalForceHTTPKeepsHTTP keeps the
// hard global override authoritative: force_http disables the Messages bridge.
func TestOpenAIGatewayService_ForwardAsAnthropic_GlobalForceHTTPKeepsHTTP(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.ForceHTTP = true
	svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, newAnthropicMessagesBridgeCaptureConn(t))
	useAnthropicMessagesBridgeSSEUpstream(upstream)
	body := anthropicMessagesBridgeTestBody(true)
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)

	result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(true, false), body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, 0, dialer.DialCount())
	require.NotNil(t, upstream.lastReq)
	require.Contains(t, rec.Body.String(), "event: ")
}

// TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeFirstOutputTimeout keeps
// the HTTP first-output deadline effective for the bridged Messages request and
// skips the pointless HTTP fallback once the deadline has already passed.
func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeFirstOutputTimeout(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
	captureConn := &openAIWSCaptureConn{
		readDelays: []time.Duration{3 * time.Second},
		events: [][]byte{
			[]byte(`{"type":"response.output_text.delta","delta":"late"}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_late","model":"gpt-6.1-sol","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`),
		},
	}
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, captureConn)
	body := anthropicMessagesBridgeTestBody(true)
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)

	startedAt := time.Now()
	result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
	elapsed := time.Since(startedAt)

	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Less(t, elapsed, 2500*time.Millisecond)
	require.Nil(t, upstream.lastReq, "deadline already passed: HTTP fallback would time out immediately")
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeBufferedDeltaRecovery(t *testing.T) {
	for _, terminalOutput := range []string{`[]`, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}]`} {
		t.Run(terminalOutput, func(t *testing.T) {
			conn := &openAIWSCaptureConn{events: [][]byte{
				[]byte(`{"type":"response.output_text.delta","delta":"ok"}`),
				[]byte(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`),
				[]byte(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"q\":\"hi\"}"}`),
				[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_delta","model":"gpt-6.1-sol","status":"completed","output":%s,"usage":{"input_tokens":5,"output_tokens":2}}}`, terminalOutput)),
			}}
			svc, upstream, _ := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), conn)
			body := anthropicMessagesBridgeTestBody(false)
			c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
			result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Nil(t, upstream.lastReq)
			require.Equal(t, "ok", gjson.Get(rec.Body.String(), "content.0.text").String())
			require.Equal(t, "tool_use", gjson.Get(rec.Body.String(), "content.1.type").String())
			require.Equal(t, "hi", gjson.Get(rec.Body.String(), "content.1.input.q").String())
		})
	}
}

func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeCyberPolicy(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, eventType := range []string{"response.failed", "error"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, eventType), func(t *testing.T) {
				event := `{"type":"response.failed","response":{"id":"resp_cyber","model":"gpt-6.1-sol","status":"failed","output":[],"error":{"code":"cyber_policy","message":"blocked by cyber policy"},"usage":{"input_tokens":9,"output_tokens":2}}}`
				if eventType == "error" {
					event = `{"type":"error","error":{"code":"cyber_policy","message":"blocked by cyber policy"},"usage":{"input_tokens":9,"output_tokens":2}}`
				}
				svc, upstream, _ := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), &openAIWSCaptureConn{events: [][]byte{[]byte(event)}})
				useAnthropicMessagesBridgeSSEUpstream(upstream)
				body := anthropicMessagesBridgeTestBody(stream)
				c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
				result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
				require.Error(t, err)
				require.Nil(t, result, "cyber-policy result must not enter normal usage billing")
				var failoverErr *UpstreamFailoverError
				require.False(t, errors.As(err, &failoverErr))
				require.Nil(t, upstream.lastReq, "cyber-policy is terminal, not an HTTP-fallback trigger")
				mark := GetOpsCyberPolicy(c)
				require.NotNil(t, mark)
				require.Equal(t, 9, mark.UpstreamInTok)
				require.Equal(t, 2, mark.UpstreamOutTok)
				require.ErrorIs(t, err, errOpenAICyberPolicyForwarded)
				if stream {
					require.Contains(t, rec.Body.String(), "event: error")
				} else {
					require.Equal(t, "error", gjson.Get(rec.Body.String(), "type").String())
					require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
					require.Contains(t, gjson.Get(rec.Body.String(), "error.message").String(), "blocked by cyber policy")
				}
			})
		}
	}
}

func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeConverterTimeoutStopsWS(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			cfg := newOpenAIWSV2TestConfig()
			cfg.Gateway.StreamDataIntervalTimeout = 1
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
			conn := &openAIWSCaptureConn{
				readDelays: []time.Duration{0, 4 * time.Second},
				events: [][]byte{
					[]byte(`{"type":"response.output_text.delta","delta":"ok"}`),
					[]byte(`{"type":"response.completed","response":{"id":"resp_timeout","model":"gpt-6.1-sol","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2}}}`),
				},
			}
			svc, upstream, _ := newFastWSBridgeTestService(t, cfg, conn)
			body := anthropicMessagesBridgeTestBody(stream)
			c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
			started := time.Now()
			_, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
			require.Error(t, err)
			require.Contains(t, err.Error(), "interval timeout")
			require.Less(t, time.Since(started), 3*time.Second, "converter timeout must not wait for the WS read timeout")
			require.Nil(t, upstream.lastReq)
		})
	}
}

func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeIsolatesSessionState(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	conn := newAnthropicMessagesBridgeCaptureConn(t)
	svc, _, dialer := newFastWSBridgeTestService(t, cfg, conn)
	body := anthropicMessagesBridgeTestBody(true)
	c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
	c.Set("api_key", &APIKey{ID: 202})
	c.Request.Header.Set("session_id", "shared_client_seed")
	// This is the unscoped slot a different API key can have populated.
	rawHash := svc.GenerateSessionHash(c, nil)
	require.NotEmpty(t, rawHash)
	svc.getOpenAIWSStateStore().BindSessionTurnState(0, rawHash, "other_api_key_turn", time.Minute)
	result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(false, false), body, "shared_client_seed", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, dialer.lastHeaders.Get(openAIWSTurnStateHeader), "another API key's WS turn state must not leak into the Messages bridge")
}

func TestApplyOpenAIWSBridgeResultFieldsUsesTurnIdentityAndRawMetadata(t *testing.T) {
	header := http.Header{"X-Request-Id": []string{"same_ws_handshake"}}
	sink := newOpenAIWSPipeDownstream(io.Discard, nil)
	sink.SetHandshakeHeaders(header)
	for _, responseID := range []string{"resp_turn_1", "resp_turn_2"} {
		converted := &OpenAIForwardResult{RequestID: "same_ws_handshake", UpstreamResponseModel: "claude-sonnet-4-6", FirstTokenMs: new(int)}
		wsResult := &OpenAIForwardResult{
			RequestID: responseID, ResponseID: responseID,
			UpstreamResponseModel: "gpt-6.1-sol", UpstreamResponseModelConflict: true,
			UpstreamResponseServiceTier: "default",
		}
		applyOpenAIWSBridgeResultFields(converted, wsResult, sink)
		require.Equal(t, responseID, converted.RequestID, "WS billing identity must be per turn, not per handshake")
		require.Equal(t, "same_ws_handshake", converted.UpstreamHeaders.Get("x-request-id"))
		require.Equal(t, "gpt-6.1-sol", converted.UpstreamResponseModel)
		require.True(t, converted.UpstreamResponseModelConflict)
		require.Equal(t, "default", converted.UpstreamResponseServiceTier)
		require.Nil(t, converted.FirstTokenMs, "a terminal-only turn has no semantic first-token timestamp")
	}
}

func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeTopLevelTerminalUsage(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.output_text.delta","delta":"ok"}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_usage","model":"gpt-6.1-sol","status":"completed","output":[]},"usage":{"input_tokens":13,"output_tokens":3}}`),
	}}
	svc, _, _ := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), conn)
	body := anthropicMessagesBridgeTestBody(false)
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
	result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 13, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Equal(t, int64(13), gjson.Get(rec.Body.String(), "usage.input_tokens").Int())
	require.Equal(t, int64(3), gjson.Get(rec.Body.String(), "usage.output_tokens").Int())
}

func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeRawResponseObservation(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			conn := newAnthropicMessagesBridgeCaptureConn(t)
			conn.events[2] = []byte(strings.Replace(string(conn.events[2]), `"status":"completed"`, `"service_tier":"default","status":"completed"`, 1))
			svc, _, dialer := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), conn)
			dialer.handshake = http.Header{"X-Request-Id": []string{"rid_shared_handshake"}}
			body := []byte(strings.Replace(string(anthropicMessagesBridgeTestBody(stream)), "gpt-6.1-sol", "claude-sonnet-4-6", 1))
			c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
			result, err := svc.ForwardAsAnthropic(fastWSBridgeTestContext(OpenAIFastTierPriority), c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "gpt-6.1-sol", result.UpstreamResponseModel)
			require.False(t, result.UpstreamResponseModelConflict)
			require.Equal(t, "resp_msg_ws", result.RequestID)
			require.Equal(t, "rid_shared_handshake", result.UpstreamHeaders.Get("x-request-id"))
			require.Equal(t, "default", result.UpstreamResponseServiceTier)
			require.NotNil(t, result.ServiceTier)
			require.Equal(t, OpenAIFastTierPriority, *result.ServiceTier, "Codex outbound fast tier remains separate from its default echo")
			require.Contains(t, rec.Body.String(), "claude-sonnet-4-6")
		})
	}
}

type anthropicMessagesBridgeCancelConn struct {
	*openAIWSCaptureConn
	cancel   context.CancelFunc
	cancelAt int
	reads    int
}

func (c *anthropicMessagesBridgeCancelConn) ReadMessage(ctx context.Context) ([]byte, error) {
	c.reads++
	if c.reads == c.cancelAt {
		// Cancel before returning the next event, so only the WS drain sees it.
		c.cancel()
	}
	return c.openAIWSCaptureConn.ReadMessage(ctx)
}

func TestOpenAIGatewayService_ForwardAsAnthropic_FastBridgeCanceledDrainUsage(t *testing.T) {
	for _, tc := range []struct {
		stream   bool
		cancelAt int
	}{{true, 2}, {true, 3}, {false, 2}, {false, 3}} {
		stream, readToCancel := tc.stream, tc.cancelAt
		t.Run(fmt.Sprintf("stream=%v/cancelAt=%d", stream, readToCancel), func(t *testing.T) {
			cfg := newOpenAIWSV2TestConfig()
			ctx, cancel := context.WithCancel(fastWSBridgeTestContext(OpenAIFastTierPriority))
			defer cancel()
			capture := newAnthropicMessagesBridgeCaptureConn(t)
			capture.events[2] = []byte(`{"type":"response.completed","response":{"id":"resp_msg_ws","model":"gpt-6.1-sol","service_tier":"default","status":"completed","output":[],"usage":{"input_tokens":17,"output_tokens":4,"input_tokens_details":{"cached_tokens":6}}}}`)
			svc, upstream, _ := newFastWSBridgeTestService(t, cfg, capture)
			useAnthropicMessagesBridgeSSEUpstream(upstream)
			useFastWSBridgeTestDialer(svc, cfg, &sequenceFastWSBridgeDialer{conns: []openAIWSClientConn{&anthropicMessagesBridgeCancelConn{openAIWSCaptureConn: capture, cancel: cancel, cancelAt: readToCancel}}})
			body := anthropicMessagesBridgeTestBody(stream)
			c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
			c.Request = c.Request.WithContext(ctx)
			result, err := svc.ForwardAsAnthropic(ctx, c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
			require.NotNil(t, result)
			require.Equal(t, 17, result.Usage.InputTokens)
			require.Equal(t, 4, result.Usage.OutputTokens)
			require.Equal(t, 6, result.Usage.CacheReadInputTokens)
			require.True(t, result.ClientDisconnect)
			require.Equal(t, "default", result.UpstreamResponseServiceTier)
			require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
			require.Nil(t, upstream.lastReq, "a canceled request must not be replayed via HTTP")
			require.Equal(t, stream, result.Stream)
			if readToCancel == 3 {
				require.Error(t, err, "downstream did not receive a terminal event")
			}
		})
	}
}
