package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestOpenAIWSLatencyStages(t *testing.T) {
	start := time.Now().Add(-3 * time.Second)
	semanticMs := 1700
	latency := &openAIWSLatency{
		start: start, acquireStart: start.Add(100 * time.Millisecond),
		acquireEnd: start.Add(400 * time.Millisecond),
		writeStart: start.Add(450 * time.Millisecond), writeEnd: start.Add(500 * time.Millisecond),
		acquired: true, connReused: true, connPick: 2 * time.Millisecond, queueWait: 5 * time.Millisecond,
		firstSemanticMs: &semanticMs,
	}
	latency.observeEvent(start.Add(600*time.Millisecond), "response.created", []byte(`{"type":"response.created"}`))
	latency.observeEvent(start.Add(1700*time.Millisecond), "response.reasoning_summary_text.delta", []byte(`{"delta":"private-reasoning-sentinel"}`))
	latency.observeEvent(start.Add(1800*time.Millisecond), "response.output_text.delta", []byte(`{"delta":""}`))
	latency.observeEvent(start.Add(1900*time.Millisecond), "response.output_text.delta", []byte(`{"delta":"private-text-sentinel"}`))
	latency.observeEvent(start.Add(2000*time.Millisecond), "response.output_text.delta", []byte(`{"delta":"later"}`))
	core, observed := observer.New(zap.InfoLevel)
	latency.log(logger.IntoContext(context.Background(), zap.New(core)), nil, nil, true)
	require.Len(t, observed.All(), 1)
	entry := observed.All()[0]
	require.Equal(t, "openai.messages_ws_latency", entry.Message)
	fields := entry.ContextMap()
	for key, value := range map[string]int64{
		"prep_ms": 100, "acquire_ms": 300, "prewrite_ms": 50, "write_ms": 50,
		"upstream_first_event_wait_ms": 100, "upstream_first_semantic_wait_ms": 1200,
		"first_text_ms": 1900, "first_semantic_ms": 1700, "conn_pick_ms": 2, "queue_wait_ms": 5,
	} {
		require.Equal(t, value, fields[key], key)
	}
	require.Equal(t, true, fields["conn_reused"])
	require.Equal(t, "completed", fields["outcome"])
	require.NotContains(t, entry.ContextMap(), "delta")
	require.NotContains(t, entry.Context, "private-text-sentinel")
}

func TestOpenAIWSLatencyMissingStagesAndLevels(t *testing.T) {
	for _, tt := range []struct {
		name     string
		semantic *int
		elapsed  time.Duration
		err      error
		handled  bool
		canceled bool
		want     string
	}{
		{name: "fast", semantic: new(int), handled: true},
		{name: "slow semantic", semantic: latencyTestInt(1000), handled: true, want: "completed"},
		{name: "long stream fast first", semantic: latencyTestInt(10), elapsed: 2 * time.Second, handled: true},
		{name: "terminal only slow", elapsed: 2 * time.Second, handled: true, want: "completed"},
		{name: "terminal only fast", handled: true},
		{name: "error", err: errors.New("private-error-sentinel"), handled: true, want: "failed"},
		{name: "fallback", handled: false, want: "http_fallback"},
		{name: "canceled", canceled: true, handled: true, want: "canceled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			core, observed := observer.New(zap.InfoLevel)
			ctx, cancel := context.WithCancel(logger.IntoContext(context.Background(), zap.New(core)))
			defer cancel()
			if tt.canceled {
				cancel()
			}
			latency := &openAIWSLatency{start: time.Now().Add(-tt.elapsed), firstSemanticMs: tt.semantic}
			latency.log(ctx, nil, tt.err, tt.handled)
			if tt.want == "" {
				require.Empty(t, observed.All())
				return
			}
			require.Len(t, observed.All(), 1)
			fields := observed.All()[0].ContextMap()
			require.Equal(t, tt.want, fields["outcome"])
			for _, key := range []string{"acquire_ms", "write_ms", "conn_reused", "first_text_ms", "upstream_first_event_wait_ms"} {
				require.NotContains(t, fields, key)
			}
			if tt.semantic == nil {
				require.NotContains(t, fields, "first_semantic_ms")
			}
		})
	}
	core, observed := observer.New(zap.DebugLevel)
	latency := &openAIWSLatency{start: time.Now()}
	latency.log(logger.IntoContext(context.Background(), zap.New(core)), nil, nil, true)
	require.Len(t, observed.All(), 1)
	require.Equal(t, zap.DebugLevel, observed.All()[0].Level)
}

func latencyTestInt(v int) *int { return &v }

func TestOpenAIWSLatencyFailedTerminal(t *testing.T) {
	for _, eventType := range []string{"error", "response.failed", "response.incomplete", "response.canceled", "response.cancelled"} {
		t.Run(eventType, func(t *testing.T) {
			latency := &openAIWSLatency{start: time.Now()}
			latency.observeEvent(time.Now(), eventType, nil)
			core, observed := observer.New(zap.InfoLevel)
			latency.log(logger.IntoContext(context.Background(), zap.New(core)), nil, nil, true)
			require.Len(t, observed.All(), 1)
			want := "failed"
			if latency.upstreamCanceled {
				want = "canceled"
			}
			require.Equal(t, want, observed.All()[0].ContextMap()["outcome"])
		})
	}
}

func TestOpenAIWSLatencyNoTextForToolOrTerminal(t *testing.T) {
	latency := &openAIWSLatency{start: time.Now()}
	latency.observeEvent(time.Now(), "response.function_call_arguments.delta", []byte(`{"delta":"{}"}`))
	latency.observeEvent(time.Now(), "response.completed", []byte(`{"type":"response.completed"}`))
	require.False(t, latency.firstSemantic.IsZero())
	require.True(t, latency.firstText.IsZero())
	var disabled *openAIWSLatency
	disabled.observeEvent(time.Time{}, "response.output_text.delta", nil)
	require.Nil(t, openAIWSLatencyFromContext(context.Background()))
}
