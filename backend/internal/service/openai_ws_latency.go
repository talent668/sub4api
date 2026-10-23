package service

import (
	"context"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type openAIWSLatencyContextKey struct{}

// Only the WS producer writes these fields. The Messages bridge reads them after
// joining the producer, including on fallback and cancellation paths.
type openAIWSLatency struct {
	start            time.Time
	acquireStart     time.Time
	acquireEnd       time.Time
	writeStart       time.Time
	writeEnd         time.Time
	firstEvent       time.Time
	firstSemantic    time.Time
	firstText        time.Time
	firstSemanticMs  *int
	upstreamFailed   bool
	upstreamCanceled bool
	acquired         bool
	connReused       bool
	forceNewConn     bool
	connPick         time.Duration
	queueWait        time.Duration
	diagnostic       *openAIWSDiagnosticMetadata
}

func openAIWSLatencyFromContext(ctx context.Context) *openAIWSLatency {
	if ctx == nil {
		return nil
	}
	latency, _ := ctx.Value(openAIWSLatencyContextKey{}).(*openAIWSLatency)
	return latency
}

func (l *openAIWSLatency) observeEvent(receivedAt time.Time, eventType string, message []byte) {
	if l == nil {
		return
	}
	switch eventType {
	case "error", "response.failed", "response.incomplete":
		l.upstreamFailed = true
	case "response.canceled", "response.cancelled":
		l.upstreamCanceled = true
	}
	l.diagnostic.observeEvent(eventType, message)
	if l.firstEvent.IsZero() {
		l.firstEvent = receivedAt
	}
	if l.firstSemantic.IsZero() && isOpenAIWSTokenEvent(eventType) {
		l.firstSemantic = receivedAt
	}
	if l.firstText.IsZero() && eventType == "response.output_text.delta" && gjson.GetBytes(message, "delta").String() != "" {
		l.firstText = receivedAt
	}
}

func (l *openAIWSLatency) log(ctx context.Context, result *OpenAIForwardResult, err error, handled bool) {
	outcome := "completed"
	if !handled {
		outcome = "http_fallback"
	} else if ctx.Err() != nil || l.upstreamCanceled || (result != nil && result.ClientDisconnect) {
		outcome = "canceled"
	} else if err != nil || l.upstreamFailed {
		outcome = "failed"
	}
	elapsed := time.Since(l.start)
	slow := elapsed >= time.Second
	if l.firstSemanticMs != nil {
		slow = *l.firstSemanticMs >= 1000
	}
	log := logger.FromContext(ctx)
	level := zap.DebugLevel
	if slow || outcome != "completed" {
		level = zap.InfoLevel
	}
	entry := log.Check(level, "openai.messages_ws_latency")
	persist := logger.RequestDiagnosticsEnabled()
	if entry == nil && !persist {
		return
	}
	fields := []zap.Field{
		zap.String("ingress", "messages_http"),
		zap.String("transport", "websocket"),
		zap.String("outcome", outcome),
		zap.Int64("attempt_duration_ms", elapsed.Milliseconds()),
		zap.Bool("force_new_conn", l.forceNewConn),
	}
	addDuration := func(name string, start, end time.Time) {
		if !start.IsZero() && !end.IsZero() {
			fields = append(fields, zap.Int64(name, end.Sub(start).Milliseconds()))
		}
	}
	addDuration("prep_ms", l.start, l.acquireStart)
	addDuration("acquire_ms", l.acquireStart, l.acquireEnd)
	addDuration("prewrite_ms", l.acquireEnd, l.writeStart)
	addDuration("write_ms", l.writeStart, l.writeEnd)
	addDuration("upstream_first_event_wait_ms", l.writeEnd, l.firstEvent)
	addDuration("upstream_first_semantic_wait_ms", l.writeEnd, l.firstSemantic)
	addDuration("first_text_ms", l.start, l.firstText)
	if l.firstSemanticMs != nil {
		fields = append(fields, zap.Int("first_semantic_ms", *l.firstSemanticMs))
	}
	if l.acquired {
		fields = append(fields,
			zap.Bool("conn_reused", l.connReused),
			zap.Int64("conn_pick_ms", l.connPick.Milliseconds()),
			zap.Int64("queue_wait_ms", l.queueWait.Milliseconds()),
		)
	}
	if persist {
		persisted := logger.SafeRequestDiagnosticContextFields(ctx)
		encoder := zapcore.NewMapObjectEncoder()
		for _, field := range fields {
			field.AddTo(encoder)
		}
		for key, value := range encoder.Fields {
			persisted[key] = value
		}
		persisted["event"] = logger.MessagesWSLatencyEvent
		// Expanded metadata goes only to the direct diagnostic sink. Ordinary
		// stdout/debug summaries retain their existing latency-only contract.
		if l.diagnostic != nil {
			for key, value := range logger.ProjectRequestDiagnosticSnapshots(l.diagnostic.sections) {
				persisted[key] = value
			}
		}
		logger.WriteSinkEvent("info", logger.RequestDiagnosticsComponent, logger.MessagesWSLatencyEvent, persisted)
	}
	if entry != nil {
		entry.Write(append(fields, zap.Bool(logger.OpsSystemLogSkipField, true))...)
	}
}
