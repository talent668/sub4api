package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const messagesWSLatencyLogMessage = "openai.messages_ws_latency"

func messagesWSLatencyObservedContext() (context.Context, *observer.ObservedLogs) {
	core, observed := observer.New(zap.DebugLevel)
	ctx := logger.IntoContext(fastWSBridgeTestContext(OpenAIFastTierPriority), zap.New(core).With(zap.String("test_request", "messages-latency")))
	return ctx, observed
}

func requireMessagesWSLatencyEntry(t *testing.T, observed *observer.ObservedLogs, level zapcore.Level, outcome string) observer.LoggedEntry {
	t.Helper()
	entries := observed.FilterMessage(messagesWSLatencyLogMessage).All()
	require.Len(t, entries, 1, "the bridge must log once, after its WS producer has joined")
	entry := entries[0]
	require.Equal(t, level, entry.Level)
	fields := entry.ContextMap()
	require.Equal(t, "messages-latency", fields["test_request"], "use the request's logger, not the global logger")
	require.Equal(t, "messages_http", fields["ingress"])
	require.Equal(t, "websocket", fields["transport"])
	require.Equal(t, outcome, fields["outcome"])
	return entry
}

func requireMessagesWSLatencyMillis(t *testing.T, fields map[string]any, key string) int64 {
	t.Helper()
	value, ok := fields[key].(int64)
	require.True(t, ok, "%s should be a recorded integer duration, got %v", key, fields[key])
	require.GreaterOrEqual(t, value, int64(0), key)
	return value
}

func requireMessagesWSLatencyPrivate(t *testing.T, entry observer.LoggedEntry, sentinels ...string) {
	t.Helper()
	// Check actual encoded values, not []zap.Field membership: the latter can
	// miss a secret stored in a field's String/Interface member.
	encoded, err := json.Marshal(entry.ContextMap())
	require.NoError(t, err)
	for _, sentinel := range sentinels {
		require.NotContains(t, entry.Message+string(encoded), sentinel)
	}
	allowed := map[string]bool{
		"test_request": true, "ingress": true, "transport": true, "outcome": true,
		"attempt_duration_ms": true, "prep_ms": true, "acquire_ms": true,
		"prewrite_ms": true, "write_ms": true, "upstream_first_event_wait_ms": true,
		"upstream_first_semantic_wait_ms": true, "first_semantic_ms": true,
		"first_text_ms": true, "conn_reused": true, "force_new_conn": true,
		"conn_pick_ms": true, "queue_wait_ms": true,
		"platform": true, "model": true, "account_id": true, "api_key_id": true,
		"user_id": true, "request_id": true, "client_request_id": true,
		logger.OpsSystemLogSkipField: true,
	}
	for key := range entry.ContextMap() {
		require.True(t, allowed[key], "unexpected latency log field %q could expose request data", key)
	}
}

// Call the real Messages bridge directly only when the test needs a synthetic
// request start or a sub-second first-output deadline. Public entry-point tests
// below separately lock the selection and fallback behavior.
func forwardMessagesWSLatencyBridge(
	t *testing.T,
	svc *OpenAIGatewayService,
	ctx context.Context,
	c *gin.Context,
	start time.Time,
	firstOutputTimeout time.Duration,
) (*OpenAIForwardResult, error, bool) {
	t.Helper()
	return svc.forwardAnthropicMessagesViaFastWS(
		ctx, c, fastWSBridgeTestAccount(true, false), "latency-direct-token",
		map[string]any{
			"model": "gpt-6.1-sol", "stream": true, "store": false,
			"instructions": "test", "service_tier": OpenAIFastTierPriority,
			"input": []any{map[string]any{"type": "input_text", "text": "hi"}},
		},
		"latency-direct-session",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2, Reason: "latency_integration_test"},
		false, true, "gpt-6.1-sol", "gpt-6.1-sol", "gpt-6.1-sol", start, "", firstOutputTimeout,
	)
}

func TestMessagesWSLatencyIntegration_OneContextSummaryAndNoSecrets(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			cfg := newOpenAIWSV2TestConfig()
			cfg.Gateway.OpenAIWS.StoreDisabledConnMode = "strict"
			conn := newAnthropicMessagesBridgeCaptureConn(t)
			const outputSecret = "latency-output-secret-sentinel"
			for i, event := range conn.events {
				conn.events[i] = []byte(strings.ReplaceAll(string(event), `"ok"`, `"`+outputSecret+`"`))
			}
			svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, conn)
			t.Cleanup(svc.openaiWSPool.Close)
			dialer.handshake = http.Header{
				"X-Request-Id": []string{"latency-handshake-secret-sentinel"},
				"Set-Cookie":   []string{"latency-cookie-secret-sentinel"},
			}
			body := []byte(strings.ReplaceAll(string(anthropicMessagesBridgeTestBody(stream)), `"hi"`, `"latency-prompt-secret-sentinel"`))
			c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
			c.Request.Header.Set("Authorization", "Bearer latency-client-auth-secret-sentinel")
			c.Request.Header.Set("session_id", "latency-session-secret-sentinel")
			c.Request.Header.Set(openAIWSTurnStateHeader, "latency-turn-state-secret-sentinel")
			account := fastWSBridgeTestAccount(false, false)
			account.Credentials["access_token"] = "latency-oauth-secret-sentinel"
			ctx, observed := messagesWSLatencyObservedContext()
			c.Request = c.Request.WithContext(ctx)

			result, err := svc.ForwardAsAnthropic(ctx, c, account, body, "latency-cache-key-secret-sentinel", "gpt-6.1-sol")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.OpenAIWSMode)
			require.Nil(t, upstream.lastReq)
			require.Equal(t, 1, dialer.DialCount())
			require.Contains(t, rec.Body.String(), outputSecret)
			require.Nil(t, openAIWSLatencyFromContext(ctx), "the collector must stay private to this bridge attempt")

			entry := requireMessagesWSLatencyEntry(t, observed, zap.DebugLevel, "completed")
			fields := entry.ContextMap()
			for _, key := range []string{
				"attempt_duration_ms", "prep_ms", "acquire_ms", "prewrite_ms", "write_ms",
				"upstream_first_event_wait_ms", "upstream_first_semantic_wait_ms", "first_text_ms",
				"conn_pick_ms", "queue_wait_ms",
			} {
				requireMessagesWSLatencyMillis(t, fields, key)
			}
			require.NotNil(t, result.FirstTokenMs)
			require.Equal(t, int64(*result.FirstTokenMs), fields["first_semantic_ms"], "reuse the forwarder's existing semantic TTFT")
			require.Equal(t, false, fields["conn_reused"])
			require.Equal(t, true, fields["force_new_conn"])
			requireMessagesWSLatencyPrivate(t, entry,
				outputSecret, "latency-handshake-secret-sentinel", "latency-cookie-secret-sentinel",
				"latency-prompt-secret-sentinel", "latency-client-auth-secret-sentinel",
				"latency-session-secret-sentinel", "latency-turn-state-secret-sentinel",
				"latency-oauth-secret-sentinel", "latency-cache-key-secret-sentinel",
			)
		})
	}
}

// These small, context-aware gates exercise the production Acquire/WriteJSON
// instrumentation without using a real upstream or sleeping for the slow-log
// threshold. A captured collector is inspected only after the producer joins.
type messagesWSLatencyDialer struct {
	base    openAIWSClientDialer
	delay   time.Duration
	err     error
	latency *openAIWSLatency
}

func (d *messagesWSLatencyDialer) Dial(ctx context.Context, url string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	d.latency = openAIWSLatencyFromContext(ctx)
	if err := messagesWSLatencyWait(ctx, d.delay); err != nil {
		return nil, 0, nil, err
	}
	if d.err != nil {
		return nil, 0, nil, d.err
	}
	return d.base.Dial(ctx, url, headers, proxy)
}

type messagesWSLatencyWriteConn struct {
	*openAIWSCaptureConn
	delay time.Duration
}

func (c *messagesWSLatencyWriteConn) WriteJSON(ctx context.Context, value any) error {
	if err := messagesWSLatencyWait(ctx, c.delay); err != nil {
		return err
	}
	return c.openAIWSCaptureConn.WriteJSON(ctx, value)
}

func messagesWSLatencyWait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestMessagesWSLatencyIntegration_StagesAndSemanticBeforeText(t *testing.T) {
	for _, semanticEvent := range []string{
		`{"type":"response.reasoning_summary_text.delta","delta":"latency-reasoning-secret-sentinel"}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"latency-tool-secret-sentinel"}`,
	} {
		name := "reasoning"
		if strings.Contains(semanticEvent, "function_call") {
			name = "tool"
		}
		t.Run(name, func(t *testing.T) {
			cfg := newOpenAIWSV2TestConfig()
			conn := newAnthropicMessagesBridgeCaptureConn(t)
			conn.events = [][]byte{
				conn.events[0],
				[]byte(semanticEvent),
				[]byte(`{"type":"response.output_text.delta","delta":""}`),
				conn.events[1],
				conn.events[2],
			}
			conn.readDelays = []time.Duration{20 * time.Millisecond, 20 * time.Millisecond, 0, 30 * time.Millisecond, 0}
			svc, upstream, _ := newFastWSBridgeTestService(t, cfg, conn)
			t.Cleanup(svc.openaiWSPool.Close)
			dialer := &messagesWSLatencyDialer{
				delay: 20 * time.Millisecond,
				base: &sequenceFastWSBridgeDialer{conns: []openAIWSClientConn{
					&messagesWSLatencyWriteConn{openAIWSCaptureConn: conn, delay: 20 * time.Millisecond},
				}},
			}
			useFastWSBridgeTestDialer(svc, cfg, dialer)
			t.Cleanup(svc.openaiWSPool.Close)
			ctx, observed := messagesWSLatencyObservedContext()
			body := anthropicMessagesBridgeTestBody(true)
			c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
			c.Request = c.Request.WithContext(ctx)

			result, err := svc.ForwardAsAnthropic(ctx, c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Nil(t, upstream.lastReq)
			require.NotNil(t, dialer.latency, "the latency collector must propagate into pool acquisition")
			entry := requireMessagesWSLatencyEntry(t, observed, zap.DebugLevel, "completed")
			fields := entry.ContextMap()
			acquire := requireMessagesWSLatencyMillis(t, fields, "acquire_ms")
			write := requireMessagesWSLatencyMillis(t, fields, "write_ms")
			firstEvent := requireMessagesWSLatencyMillis(t, fields, "upstream_first_event_wait_ms")
			semanticWait := requireMessagesWSLatencyMillis(t, fields, "upstream_first_semantic_wait_ms")
			semantic := requireMessagesWSLatencyMillis(t, fields, "first_semantic_ms")
			text := requireMessagesWSLatencyMillis(t, fields, "first_text_ms")
			// Leave generous slack for timer rounding and shared CI scheduling.
			require.GreaterOrEqual(t, acquire, int64(10))
			require.GreaterOrEqual(t, write, int64(10))
			require.GreaterOrEqual(t, firstEvent, int64(10))
			require.GreaterOrEqual(t, semanticWait-firstEvent, int64(10))
			require.GreaterOrEqual(t, text-semantic, int64(10), "neither reasoning/tool deltas nor an empty text delta are first text")
			require.GreaterOrEqual(t, semantic, acquire+write+semanticWait-2)
			require.NotNil(t, result.FirstTokenMs)
			require.Equal(t, int64(*result.FirstTokenMs), semantic)
			requireMessagesWSLatencyPrivate(t, entry, "latency-reasoning-secret-sentinel", "latency-tool-secret-sentinel")
		})
	}
}

func TestMessagesWSLatencyIntegration_SlowSemanticAndTerminalOnly(t *testing.T) {
	for _, tc := range []struct {
		name         string
		terminalOnly bool
		age          time.Duration
		level        zapcore.Level
	}{
		{name: "slow_semantic", age: 2 * time.Second, level: zap.InfoLevel},
		{name: "fast_terminal_only", terminalOnly: true, level: zap.DebugLevel},
		{name: "slow_terminal_only", terminalOnly: true, age: 2 * time.Second, level: zap.InfoLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newAnthropicMessagesBridgeCaptureConn(t)
			if tc.terminalOnly {
				// The completed payload contains visible text, but there was no
				// semantic/text arrival milestone in the WS stream.
				conn.events = conn.events[2:]
			}
			svc, upstream, _ := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), conn)
			t.Cleanup(svc.openaiWSPool.Close)
			ctx, observed := messagesWSLatencyObservedContext()
			c, _ := newAnthropicMessagesBridgeTestGinContext(t, anthropicMessagesBridgeTestBody(true))
			c.Request = c.Request.WithContext(ctx)
			result, err, handled := forwardMessagesWSLatencyBridge(t, svc, ctx, c, time.Now().Add(-tc.age), 0)
			require.NoError(t, err)
			require.True(t, handled)
			require.NotNil(t, result)
			require.Nil(t, upstream.lastReq)
			entry := requireMessagesWSLatencyEntry(t, observed, tc.level, "completed")
			fields := entry.ContextMap()
			requireMessagesWSLatencyMillis(t, fields, "upstream_first_event_wait_ms")
			if tc.terminalOnly {
				require.Nil(t, result.FirstTokenMs)
				for _, key := range []string{"first_semantic_ms", "upstream_first_semantic_wait_ms", "first_text_ms"} {
					require.NotContains(t, fields, key, "missing milestones must be absent, not fabricated zeroes")
				}
			} else {
				require.GreaterOrEqual(t, requireMessagesWSLatencyMillis(t, fields, "first_semantic_ms"), int64(2000))
			}
			if tc.age > 0 {
				require.GreaterOrEqual(t, requireMessagesWSLatencyMillis(t, fields, "attempt_duration_ms"), int64(2000))
			}
		})
	}
}

func TestMessagesWSLatencyIntegration_HTTPFallbackOmitsUnacquiredConnectionStats(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	svc, upstream, _ := newFastWSBridgeTestService(t, cfg, newAnthropicMessagesBridgeCaptureConn(t))
	t.Cleanup(svc.openaiWSPool.Close)
	useAnthropicMessagesBridgeSSEUpstream(upstream)
	dialer := &messagesWSLatencyDialer{err: errors.New("latency-dial-error-secret-sentinel")}
	useFastWSBridgeTestDialer(svc, cfg, dialer)
	t.Cleanup(svc.openaiWSPool.Close)
	ctx, observed := messagesWSLatencyObservedContext()
	body := anthropicMessagesBridgeTestBody(true)
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, body)
	c.Request = c.Request.WithContext(ctx)

	result, err := svc.ForwardAsAnthropic(ctx, c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.NotNil(t, upstream.lastReq, "the original HTTP path should still complete")
	require.Contains(t, rec.Body.String(), "event: ")
	entry := requireMessagesWSLatencyEntry(t, observed, zap.InfoLevel, "http_fallback")
	fields := entry.ContextMap()
	requireMessagesWSLatencyMillis(t, fields, "prep_ms")
	requireMessagesWSLatencyMillis(t, fields, "acquire_ms")
	for _, key := range []string{
		"conn_reused", "conn_pick_ms", "queue_wait_ms", "prewrite_ms", "write_ms",
		"upstream_first_event_wait_ms", "upstream_first_semantic_wait_ms", "first_semantic_ms", "first_text_ms",
	} {
		require.NotContains(t, fields, key, "failed acquire has no connection or write/read measurements")
	}
	requireMessagesWSLatencyPrivate(t, entry, "latency-dial-error-secret-sentinel")
}

func TestMessagesWSLatencyIntegration_FirstOutputTimeoutIsFailed(t *testing.T) {
	conn := newAnthropicMessagesBridgeCaptureConn(t)
	conn.readDelays = []time.Duration{200 * time.Millisecond}
	svc, upstream, _ := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), conn)
	t.Cleanup(svc.openaiWSPool.Close)
	ctx, observed := messagesWSLatencyObservedContext()
	c, rec := newAnthropicMessagesBridgeTestGinContext(t, anthropicMessagesBridgeTestBody(true))
	c.Request = c.Request.WithContext(ctx)
	started := time.Now()
	result, err, handled := forwardMessagesWSLatencyBridge(t, svc, ctx, c, started, 25*time.Millisecond)
	require.True(t, handled)
	require.Nil(t, result)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Equal(t, http.StatusGatewayTimeout, failover.StatusCode)
	require.Less(t, time.Since(started), 2*time.Second, "the fake's pending read must be canceled at the first-output deadline")
	require.Nil(t, upstream.lastReq)
	require.Empty(t, rec.Body.String())
	entry := requireMessagesWSLatencyEntry(t, observed, zap.InfoLevel, "failed")
	fields := entry.ContextMap()
	for _, key := range []string{"acquire_ms", "prewrite_ms", "write_ms", "conn_pick_ms", "queue_wait_ms"} {
		requireMessagesWSLatencyMillis(t, fields, key)
	}
	for _, key := range []string{"upstream_first_event_wait_ms", "upstream_first_semantic_wait_ms", "first_semantic_ms", "first_text_ms"} {
		require.NotContains(t, fields, key)
	}
}

func TestMessagesWSLatencyIntegration_CanceledDrainLogsOnceAfterUsage(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			cfg := newOpenAIWSV2TestConfig()
			ctx, observed := messagesWSLatencyObservedContext()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			conn := newAnthropicMessagesBridgeCaptureConn(t)
			svc, upstream, _ := newFastWSBridgeTestService(t, cfg, conn)
			t.Cleanup(svc.openaiWSPool.Close)
			useFastWSBridgeTestDialer(svc, cfg, &sequenceFastWSBridgeDialer{conns: []openAIWSClientConn{
				&anthropicMessagesBridgeCancelConn{openAIWSCaptureConn: conn, cancel: cancel, cancelAt: 3},
			}})
			t.Cleanup(svc.openaiWSPool.Close)
			body := anthropicMessagesBridgeTestBody(stream)
			c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
			c.Request = c.Request.WithContext(ctx)
			result, _ := svc.ForwardAsAnthropic(ctx, c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
			require.NotNil(t, result)
			require.True(t, result.ClientDisconnect)
			require.Equal(t, 5, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.OutputTokens)
			require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
			require.Nil(t, upstream.lastReq)
			entry := requireMessagesWSLatencyEntry(t, observed, zap.InfoLevel, "canceled")
			require.NotNil(t, result.FirstTokenMs)
			require.Equal(t, int64(*result.FirstTokenMs), entry.ContextMap()["first_semantic_ms"])
			requireMessagesWSLatencyMillis(t, entry.ContextMap(), "first_text_ms")
		})
	}
}

func TestMessagesWSLatencyIntegration_ConnectionReuse(t *testing.T) {
	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.StoreDisabledConnMode = "off"
	conn := newAnthropicMessagesBridgeCaptureConn(t)
	conn.events = append(conn.events, newAnthropicMessagesBridgeCaptureConn(t).events...)
	svc, _, dialer := newFastWSBridgeTestService(t, cfg, conn)
	t.Cleanup(svc.openaiWSPool.Close)
	ctx, observed := messagesWSLatencyObservedContext()
	for attempt := 0; attempt < 2; attempt++ {
		c, _ := newAnthropicMessagesBridgeTestGinContext(t, anthropicMessagesBridgeTestBody(true))
		c.Request.Header.Set("session_id", "latency-stable-reuse-session")
		c.Request = c.Request.WithContext(ctx)
		result, err, handled := forwardMessagesWSLatencyBridge(t, svc, ctx, c, time.Now(), 0)
		require.NoError(t, err)
		require.True(t, handled)
		require.NotNil(t, result)
	}
	entries := observed.FilterMessage(messagesWSLatencyLogMessage).All()
	require.Len(t, entries, 2, "each bridge attempt must own one fresh collector")
	require.Equal(t, 1, dialer.DialCount())
	for i, entry := range entries {
		fields := entry.ContextMap()
		require.Equal(t, i == 1, fields["conn_reused"])
		require.Equal(t, false, fields["force_new_conn"])
		require.Equal(t, "completed", fields["outcome"])
		requireMessagesWSLatencyMillis(t, fields, "conn_pick_ms")
		requireMessagesWSLatencyMillis(t, fields, "queue_wait_ms")
	}
}

func TestMessagesWSLatencyIntegration_OnlyMessagesWSBridge(t *testing.T) {
	for _, name := range []string{"messages_http", "messages_disabled", "responses_ws"} {
		t.Run(name, func(t *testing.T) {
			cfg := newOpenAIWSV2TestConfig()
			if name == "messages_disabled" {
				cfg.Gateway.OpenAIWS.ForceHTTP = true
			}
			svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, newAnthropicMessagesBridgeCaptureConn(t))
			t.Cleanup(svc.openaiWSPool.Close)
			useAnthropicMessagesBridgeSSEUpstream(upstream)
			ctx, observed := messagesWSLatencyObservedContext()
			if name == "messages_http" {
				ctx = logger.IntoContext(context.Background(), logger.FromContext(ctx))
			}
			var result *OpenAIForwardResult
			var err error
			if name == "responses_ws" {
				c, _ := newFastWSBridgeTestGinContext(t)
				c.Request = c.Request.WithContext(ctx)
				result, err = svc.Forward(ctx, c, fastWSBridgeTestAccount(true, false), []byte(fastWSBridgeTestBody))
			} else {
				body := anthropicMessagesBridgeTestBody(true)
				c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
				c.Request = c.Request.WithContext(ctx)
				result, err = svc.ForwardAsAnthropic(ctx, c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Nil(t, openAIWSLatencyFromContext(ctx))
			require.Empty(t, observed.FilterMessage(messagesWSLatencyLogMessage).All(), "no Messages latency record outside the enabled Messages WS bridge")
			if name == "responses_ws" {
				require.True(t, result.OpenAIWSMode)
				require.Equal(t, 1, dialer.DialCount(), "ordinary Responses must tolerate a nil latency collector")
			} else {
				require.False(t, result.OpenAIWSMode)
				require.Zero(t, dialer.DialCount())
				require.NotNil(t, upstream.lastReq)
			}
		})
	}
}

func TestMessagesWSLatencyIntegration_NilRequestLogger(t *testing.T) {
	svc, upstream, _ := newFastWSBridgeTestService(t, newOpenAIWSV2TestConfig(), newAnthropicMessagesBridgeCaptureConn(t))
	t.Cleanup(svc.openaiWSPool.Close)
	ctx := logger.IntoContext(fastWSBridgeTestContext(OpenAIFastTierPriority), nil)
	body := anthropicMessagesBridgeTestBody(true)
	c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
	c.Request = c.Request.WithContext(ctx)
	result, err := svc.ForwardAsAnthropic(ctx, c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode)
	require.Nil(t, upstream.lastReq)
}
