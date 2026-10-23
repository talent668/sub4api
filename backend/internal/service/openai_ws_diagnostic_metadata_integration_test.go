package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func requireOpenAIWSDiagnosticSinkEvent(t *testing.T, sink *latencyDiagnosticCaptureSink) *logger.LogEvent {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var diagnostics []*logger.LogEvent
	for _, event := range sink.events {
		if event.Component == logger.RequestDiagnosticsComponent && event.Message == logger.MessagesWSLatencyEvent {
			diagnostics = append(diagnostics, event)
		}
	}
	require.Len(t, diagnostics, 1, "one direct diagnostic sink event after the producer joined")
	return diagnostics[0]
}

func TestMessagesWSDiagnosticIntegration_OriginalClientFinalUpstreamAndSinkOnly(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			sink := enableOpenAIWSDiagnosticTestSink(t)
			cfg := newOpenAIWSV2TestConfig()
			conn := newAnthropicMessagesBridgeCaptureConn(t)
			conn.events[2] = []byte(`{"type":"response.completed","response":{"id":"diag-response-id-secret","model":"gpt-6.1-sol","service_tier":"default","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"diag-output-secret"}]}],"usage":{"input_tokens":5,"output_tokens":2,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":1},"output_tokens_details":{"reasoning_tokens":0},"total_tokens":7}}}`)
			svc, upstream, dialer := newFastWSBridgeTestService(t, cfg, conn)
			t.Cleanup(svc.openaiWSPool.Close)
			dialer.handshake = http.Header{
				"Content-Type": {"application/json"}, "Connection": {"Upgrade"}, "Upgrade": {"websocket"},
				"X-Request-Id": {"diag-handshake-id-secret"}, "Set-Cookie": {"diag-handshake-cookie-secret"},
			}
			body := []byte(strings.ReplaceAll(strings.ReplaceAll(string(anthropicMessagesBridgeTestBody(stream)), "gpt-6.1-sol", "gpt-6.1-sol-high"), `"hi"`, `"diag-prompt-secret"`))
			c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
			c.Request.Header.Set("Accept", "application/json")
			c.Request.Header.Set("Anthropic-Version", "2023-06-01")
			c.Request.Header.Set("Authorization", "Bearer diag-client-auth-secret")
			c.Request.Header.Set("User-Agent", "diag-client-ua-secret")
			c.Request.Header.Set("session_id", "diag-session-secret")
			ctx, observed := messagesWSLatencyObservedContext()
			c.Request = c.Request.WithContext(ctx)
			result, err := svc.ForwardAsAnthropic(ctx, c, fastWSBridgeTestAccount(false, false), body, "diag-cache-key-secret", "gpt-6.1-sol")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.OpenAIWSMode)
			require.Nil(t, upstream.lastReq)
			require.Equal(t, 5, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.OutputTokens)
			fields := requireOpenAIWSDiagnosticSinkEvent(t, sink).Fields
			client := requireOpenAIWSDiagnosticSection(t, fields, "client_request")
			require.Equal(t, "gpt-6.1-sol-high", client["model"])
			require.Equal(t, stream, client["stream"])
			require.NotContains(t, client, "output_config", "normalization must not backfill the original request snapshot")
			request := requireOpenAIWSDiagnosticSection(t, fields, "upstream_request")
			require.Equal(t, conn.lastWrite["model"], request["model"])
			require.Equal(t, conn.lastWrite["service_tier"], request["service_tier"])
			require.Equal(t, "response.create", request["type"])
			require.Equal(t, true, request["stream"], "WS converter always consumes streaming events")
			require.Equal(t, false, request["store"])
			require.Equal(t, true, request["write_attempted"])
			require.Equal(t, true, request["write_completed"])
			require.Equal(t, "application/json", requireOpenAIWSDiagnosticSection(t, fields, "client_request_headers")["content_type"])
			require.Contains(t, fields, "upstream_request_headers")
			require.Equal(t, "websocket", requireOpenAIWSDiagnosticSection(t, fields, "upstream_handshake_headers")["upgrade"])
			response := requireOpenAIWSDiagnosticSection(t, fields, "response")
			require.Equal(t, "gpt-6.1-sol", response["model"], "raw upstream model, not the client-facing rewritten alias")
			require.Equal(t, "default", response["service_tier"], "observed tier, not requested priority tier")
			require.Equal(t, "response.completed", response["terminal_event"])
			require.Equal(t, map[string]any{
				"input_tokens": int64(5), "output_tokens": int64(2), "cache_read_input_tokens": int64(0),
				"cache_creation_input_tokens": int64(1), "reasoning_tokens": int64(0), "total_tokens": int64(7),
			}, requireOpenAIWSDiagnosticSection(t, fields, "usage"))
			sentinels := []string{"diag-response-id-secret", "diag-output-secret", "diag-prompt-secret", "diag-handshake-id-secret", "diag-handshake-cookie-secret", "diag-client-auth-secret", "diag-client-ua-secret", "diag-session-secret", "diag-cache-key-secret"}
			requireOpenAIWSDiagnosticNoSecrets(t, fields, sentinels...)
			entry := requireMessagesWSLatencyEntry(t, observed, zap.DebugLevel, "completed")
			requireMessagesWSLatencyPrivate(t, entry, sentinels...)
			for _, section := range []string{"client_request", "upstream_request", "client_request_headers", "upstream_request_headers", "upstream_handshake_headers", "response", "usage"} {
				require.NotContains(t, entry.ContextMap(), section, "expanded snapshots must not reach stdout")
			}
		})
	}
}

// Records source-projected state at the actual connection WriteJSON boundary.
// The prewarm write must not masquerade as a real generation request snapshot.
type openAIWSDiagnosticWriteBoundaryConn struct {
	*openAIWSCaptureConn
	snapshots []map[string]any
	writeErr  error
}

func (c *openAIWSDiagnosticWriteBoundaryConn) WriteJSON(ctx context.Context, value any) error {
	latency := openAIWSLatencyFromContext(ctx)
	var snapshot map[string]any
	if latency != nil && latency.diagnostic != nil {
		snapshot = logger.ProjectRequestDiagnosticSnapshots(latency.diagnostic.sections)
	}
	c.snapshots = append(c.snapshots, snapshot)
	if c.writeErr != nil {
		return c.writeErr
	}
	return c.openAIWSCaptureConn.WriteJSON(ctx, value)
}

func TestMessagesWSDiagnosticIntegration_PrewarmBeforeRealWriteSnapshot(t *testing.T) {
	sink := enableOpenAIWSDiagnosticTestSink(t)
	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = true
	conn := newAnthropicMessagesBridgeCaptureConn(t)
	conn.events = append([][]byte{[]byte(`{"type":"response.completed","response":{"id":"prewarm-id-secret","model":"prewarm-model","usage":{"input_tokens":999}}}`)}, conn.events...)
	boundary := &openAIWSDiagnosticWriteBoundaryConn{openAIWSCaptureConn: conn}
	svc, _, _ := newFastWSBridgeTestService(t, cfg, conn)
	t.Cleanup(svc.openaiWSPool.Close)
	useFastWSBridgeTestDialer(svc, cfg, &sequenceFastWSBridgeDialer{conns: []openAIWSClientConn{boundary}})
	t.Cleanup(svc.openaiWSPool.Close)
	ctx, _ := messagesWSLatencyObservedContext()
	body := anthropicMessagesBridgeTestBody(true)
	c, _ := newAnthropicMessagesBridgeTestGinContext(t, body)
	result, err := svc.ForwardAsAnthropic(ctx, c, fastWSBridgeTestAccount(false, false), body, "", "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, boundary.snapshots, 2)
	require.NotContains(t, boundary.snapshots[0], "upstream_request")
	request := requireOpenAIWSDiagnosticSection(t, boundary.snapshots[1], "upstream_request")
	require.Equal(t, true, request["write_attempted"])
	require.Equal(t, false, request["write_completed"], "a snapshot at WriteJSON entry is not a completed write")
	require.Equal(t, "response.create", request["type"])
	require.Equal(t, false, request["has_previous_response_id"])
	require.NotContains(t, request, "generate")
	fields := requireOpenAIWSDiagnosticSinkEvent(t, sink).Fields
	require.Equal(t, true, requireOpenAIWSDiagnosticSection(t, fields, "upstream_request")["write_completed"])
	require.EqualValues(t, 5, requireOpenAIWSDiagnosticSection(t, fields, "usage")["input_tokens"])
	requireOpenAIWSDiagnosticNoSecrets(t, fields, "prewarm-id-secret", "prewarm-model")
}

func TestMessagesWSDiagnosticIntegration_FailedWritesDoNotClaimGeneration(t *testing.T) {
	for _, prewarm := range []bool{false, true} {
		t.Run(fmt.Sprintf("prewarm=%v", prewarm), func(t *testing.T) {
			sink := enableOpenAIWSDiagnosticTestSink(t)
			cfg := newOpenAIWSV2TestConfig()
			cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = prewarm
			conn := newAnthropicMessagesBridgeCaptureConn(t)
			boundary := &openAIWSDiagnosticWriteBoundaryConn{openAIWSCaptureConn: conn, writeErr: errors.New("diag-write-error-secret")}
			svc, _, _ := newFastWSBridgeTestService(t, cfg, conn)
			t.Cleanup(svc.openaiWSPool.Close)
			useFastWSBridgeTestDialer(svc, cfg, &sequenceFastWSBridgeDialer{conns: []openAIWSClientConn{boundary}})
			t.Cleanup(svc.openaiWSPool.Close)
			ctx, _ := messagesWSLatencyObservedContext()
			c, _ := newAnthropicMessagesBridgeTestGinContext(t, anthropicMessagesBridgeTestBody(true))
			result, err, handled := forwardMessagesWSLatencyBridge(t, svc, ctx, c, time.Now(), 0)
			require.NoError(t, err)
			require.Nil(t, result)
			require.False(t, handled)
			fields := requireOpenAIWSDiagnosticSinkEvent(t, sink).Fields
			if prewarm {
				require.NotContains(t, fields, "upstream_request", "the real generation WriteJSON was never attempted")
			} else {
				request := requireOpenAIWSDiagnosticSection(t, fields, "upstream_request")
				require.Equal(t, true, request["write_attempted"])
				require.Equal(t, false, request["write_completed"])
			}
			require.NotContains(t, fields, "response")
			require.NotContains(t, fields, "usage")
			requireOpenAIWSDiagnosticNoSecrets(t, fields, "diag-write-error-secret")
		})
	}
}

func TestMessagesWSDiagnosticIntegration_CanceledDrainUsesFinalRawSnapshot(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			sink := enableOpenAIWSDiagnosticTestSink(t)
			cfg := newOpenAIWSV2TestConfig()
			ctx, _ := messagesWSLatencyObservedContext()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			conn := newAnthropicMessagesBridgeCaptureConn(t)
			conn.events[0] = []byte(`{"type":"response.created","response":{"id":"diag-drain-id-secret","model":"gpt-6.1-sol","status":"in_progress","usage":{"input_tokens":999,"output_tokens":999,"total_tokens":1998}}}`)
			conn.events[2] = []byte(`{"type":"response.completed","response":{"id":"diag-drain-id-secret","model":"gpt-6.1-sol","service_tier":"default","status":"completed","output":[],"usage":{"input_tokens":17,"output_tokens":4,"input_tokens_details":{"cached_tokens":0}}}}`)
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
			require.Equal(t, 17, result.Usage.InputTokens)
			require.Equal(t, 4, result.Usage.OutputTokens)
			require.Nil(t, upstream.lastReq)
			fields := requireOpenAIWSDiagnosticSinkEvent(t, sink).Fields
			require.Equal(t, "canceled", fields["outcome"])
			require.Equal(t, "response.completed", requireOpenAIWSDiagnosticSection(t, fields, "response")["terminal_event"])
			require.Equal(t, map[string]any{"input_tokens": int64(17), "output_tokens": int64(4), "cache_read_input_tokens": int64(0)}, requireOpenAIWSDiagnosticSection(t, fields, "usage"))
			requireOpenAIWSDiagnosticNoSecrets(t, fields, "diag-drain-id-secret")
		})
	}
}
