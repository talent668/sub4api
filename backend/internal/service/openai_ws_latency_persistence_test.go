package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type latencyDiagnosticCaptureSink struct {
	mu      sync.Mutex
	enabled bool
	events  []*logger.LogEvent
}

func (s *latencyDiagnosticCaptureSink) RequestDiagnosticsEnabled() bool { return s.enabled }
func (s *latencyDiagnosticCaptureSink) WriteLogEvent(event *logger.LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func TestOpenAIWSLatencyPersistsFastRequestWithoutDebug(t *testing.T) {
	sink := &latencyDiagnosticCaptureSink{enabled: true}
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil) })
	core, observed := observer.New(zap.WarnLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	latency := &openAIWSLatency{start: time.Now(), firstSemanticMs: latencyTestInt(0)}
	latency.log(ctx, nil, nil, true)
	require.Empty(t, observed.All())
	require.Len(t, sink.events, 1)
	event := sink.events[0]
	require.Equal(t, logger.RequestDiagnosticsComponent, event.Component)
	require.Equal(t, logger.MessagesWSLatencyEvent, event.Message)
	require.Equal(t, logger.MessagesWSLatencyEvent, event.Fields["event"])
	require.Equal(t, int64(0), event.Fields["first_semantic_ms"])
	require.Equal(t, false, event.Fields["force_new_conn"])
	require.NotContains(t, event.Fields, logger.OpsSystemLogSkipField)
}

func TestOpenAIWSLatencyPersistenceOffKeepsOrdinaryOutput(t *testing.T) {
	sink := &latencyDiagnosticCaptureSink{}
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil) })
	core, observed := observer.New(zap.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	latency := &openAIWSLatency{start: time.Now().Add(-2 * time.Second), firstSemanticMs: latencyTestInt(1500)}
	latency.log(ctx, nil, nil, true)
	require.Empty(t, sink.events)
	require.Len(t, observed.All(), 1)
	require.Equal(t, true, observed.All()[0].ContextMap()[logger.OpsSystemLogSkipField])
}
