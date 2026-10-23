package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
)

type requestDiagnosticRetentionRepoStub struct {
	OpsRepository
	maintain func(context.Context, RequestDiagnosticRetentionPolicy) (RequestDiagnosticRetentionResult, error)
}

func (r *requestDiagnosticRetentionRepoStub) MaintainRequestDiagnosticLogs(ctx context.Context, policy RequestDiagnosticRetentionPolicy) (RequestDiagnosticRetentionResult, error) {
	return r.maintain(ctx, policy)
}

func (r *requestDiagnosticRetentionRepoStub) BatchInsertSystemLogs(context.Context, []*OpsInsertSystemLogInput) (int64, error) {
	return 0, nil
}

func receiveDiagnosticRetentionPolicy(t *testing.T, calls <-chan RequestDiagnosticRetentionPolicy) RequestDiagnosticRetentionPolicy {
	t.Helper()
	select {
	case policy := <-calls:
		return policy
	case <-time.After(2 * time.Second):
		t.Fatal("diagnostic maintenance did not run")
		return RequestDiagnosticRetentionPolicy{}
	}
}

func TestRequestDiagnosticRetentionWorker_WaitsForPolicyThenCleansWithCaptureDisabled(t *testing.T) {
	calls := make(chan RequestDiagnosticRetentionPolicy, 8)
	repo := &requestDiagnosticRetentionRepoStub{maintain: func(_ context.Context, policy RequestDiagnosticRetentionPolicy) (RequestDiagnosticRetentionResult, error) {
		calls <- policy
		return RequestDiagnosticRetentionResult{}, nil
	}}
	sink := NewOpsSystemLogSink(repo)
	sink.diagnosticRetentionInterval = 20 * time.Millisecond
	sink.Start()
	t.Cleanup(sink.Stop)
	select {
	case policy := <-calls:
		t.Fatalf("unconfigured sink authorized deletion: %+v", policy)
	case <-time.After(60 * time.Millisecond):
	}
	if err := sink.SetRequestDiagnosticRetentionPolicy(RequestDiagnosticRetentionPolicy{RetentionDays: 0}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if sink.diagnosticRetentionPolicy.Load() != nil {
		t.Fatal("invalid policy configured retention")
	}
	policy := RequestDiagnosticRetentionPolicy{RetentionDays: 7, MaxSizeMB: 32}
	if err := sink.SetRequestDiagnosticRetentionPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if got := receiveDiagnosticRetentionPolicy(t, calls); got != policy {
		t.Fatalf("loaded policy = %+v, want %+v", got, policy)
	}
	if sink.RequestDiagnosticsEnabled() {
		t.Fatal("retention unexpectedly enabled capture")
	}
	if got := receiveDiagnosticRetentionPolicy(t, calls); got != policy {
		t.Fatalf("ticker policy = %+v, want %+v", got, policy)
	}
}

func TestRequestDiagnosticRetentionWorker_StartupAndPolicyChangesWakeImmediately(t *testing.T) {
	calls := make(chan RequestDiagnosticRetentionPolicy, 8)
	repo := &requestDiagnosticRetentionRepoStub{maintain: func(_ context.Context, policy RequestDiagnosticRetentionPolicy) (RequestDiagnosticRetentionResult, error) {
		calls <- policy
		return RequestDiagnosticRetentionResult{}, nil
	}}
	sink := NewOpsSystemLogSink(repo)
	sink.diagnosticRetentionInterval = time.Hour
	first := RequestDiagnosticRetentionPolicy{RetentionDays: 21, MaxSizeMB: 128}
	if err := sink.SetRequestDiagnosticRetentionPolicy(first); err != nil {
		t.Fatal(err)
	}
	sink.Start()
	sink.Start() // Must not create another flusher/maintenance worker.
	t.Cleanup(sink.Stop)
	if got := receiveDiagnosticRetentionPolicy(t, calls); got != first {
		t.Fatalf("startup policy = %+v", got)
	}
	second := RequestDiagnosticRetentionPolicy{RetentionDays: 7}
	if err := sink.SetRequestDiagnosticRetentionPolicy(second); err != nil {
		t.Fatal(err)
	}
	if got := receiveDiagnosticRetentionPolicy(t, calls); got != second {
		t.Fatalf("policy wake used stale policy = %+v", got)
	}
	for i := 0; i < 1000; i++ {
		if err := sink.SetRequestDiagnosticRetentionPolicy(second); err != nil {
			t.Fatal(err)
		}
		sink.wakeRequestDiagnosticRetention()
	}
	select {
	case got := <-calls:
		t.Fatalf("unchanged policy wake bypassed cadence: %+v", got)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestRequestDiagnosticRetentionWorker_DoesNotBlockIngestionAndStopsOnCancellation(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	repo := &requestDiagnosticRetentionRepoStub{maintain: func(ctx context.Context, _ RequestDiagnosticRetentionPolicy) (RequestDiagnosticRetentionResult, error) {
		close(entered)
		<-ctx.Done()
		close(exited)
		return RequestDiagnosticRetentionResult{}, ctx.Err()
	}}
	sink := NewOpsSystemLogSink(repo)
	if err := sink.SetRequestDiagnosticRetentionPolicy(RequestDiagnosticRetentionPolicy{RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	sink.Start()
	t.Cleanup(sink.Stop)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("maintenance not started")
	}
	writeDone := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			sink.WriteLogEvent(&logger.LogEvent{Level: "warn", Component: "app", Message: "ordinary warning"})
		}
		close(writeDone)
	}()
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("maintenance blocked logging ingestion")
	}
	stopped := make(chan struct{})
	go func() { sink.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop with cancellation")
	}
	select {
	case <-exited:
	default:
		t.Fatal("repository context was not canceled")
	}
}

func TestRequestDiagnosticRetentionWorker_ErrorBackoffAndIncompleteScanStayBounded(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "incomplete_scan"
		if fail {
			name = "error_backoff"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			repo := &requestDiagnosticRetentionRepoStub{maintain: func(context.Context, RequestDiagnosticRetentionPolicy) (RequestDiagnosticRetentionResult, error) {
				calls.Add(1)
				if fail {
					return RequestDiagnosticRetentionResult{}, errors.New("maintenance unavailable")
				}
				return RequestDiagnosticRetentionResult{ScanIncomplete: true, HasMore: true}, nil
			}}
			sink := NewOpsSystemLogSink(repo)
			sink.diagnosticRetentionInterval = time.Hour
			sink.diagnosticRetentionBackoff = time.Hour
			sink.diagnosticRetentionBackoffMax = time.Hour
			if err := sink.SetRequestDiagnosticRetentionPolicy(RequestDiagnosticRetentionPolicy{RetentionDays: 7}); err != nil {
				t.Fatal(err)
			}
			sink.Start()
			t.Cleanup(sink.Stop)
			if !waitForOpsSystemLogCondition(t, time.Second, func() bool { return calls.Load() == 1 }) {
				t.Fatal("initial retention attempt not observed")
			}
			for i := 0; i < 1000; i++ {
				sink.wakeRequestDiagnosticRetention()
			}
			time.Sleep(30 * time.Millisecond)
			if got := calls.Load(); got != 1 {
				t.Fatalf("worker retried without cadence/backoff: %d", got)
			}
			health := sink.Health().DiagnosticRetention
			if !health.PolicyConfigured || health.LastRunAt == "" {
				t.Fatalf("retention health did not record run: %+v", health)
			}
			if fail {
				if health.LastError != "maintenance unavailable" {
					t.Fatalf("retention error not exposed: %+v", health)
				}
			} else if !health.Result.ScanIncomplete || !health.Result.HasMore {
				t.Fatalf("incomplete quota pass was silently discarded: %+v", health)
			}
		})
	}
}

func TestRequestDiagnosticRetentionWorker_TimeoutAndOptionalRepository(t *testing.T) {
	done := make(chan error, 1)
	repo := &requestDiagnosticRetentionRepoStub{maintain: func(ctx context.Context, _ RequestDiagnosticRetentionPolicy) (RequestDiagnosticRetentionResult, error) {
		<-ctx.Done()
		done <- ctx.Err()
		return RequestDiagnosticRetentionResult{}, ctx.Err()
	}}
	sink := NewOpsSystemLogSink(repo)
	sink.diagnosticRetentionInterval = time.Hour
	sink.diagnosticRetentionTimeout = 20 * time.Millisecond
	if err := sink.SetRequestDiagnosticRetentionPolicy(RequestDiagnosticRetentionPolicy{RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	sink.Start()
	t.Cleanup(sink.Stop)
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("maintenance timeout = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance call has no bounded timeout")
	}

	// Existing Ops repositories without the optional interface still work.
	legacy := NewOpsSystemLogSink(&opsRepoMock{})
	if err := legacy.SetRequestDiagnosticRetentionPolicy(RequestDiagnosticRetentionPolicy{RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	legacy.Start()
	legacy.Stop()
}

func TestRequestDiagnosticRetentionBackoffFor(t *testing.T) {
	sink := &OpsSystemLogSink{diagnosticRetentionBackoff: time.Second, diagnosticRetentionBackoffMax: 8 * time.Second}
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{{0, time.Second}, {1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {4, 8 * time.Second}, {1000, 8 * time.Second}} {
		if got := sink.requestDiagnosticRetentionBackoffFor(tc.failures); got != tc.want {
			t.Fatalf("backoff(%d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
	if got := (&OpsSystemLogSink{}).requestDiagnosticRetentionBackoffFor(1000); got != requestDiagnosticRetentionBackoffMax {
		t.Fatalf("default cap = %s", got)
	}
}
