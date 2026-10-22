package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
)

type scheduledTestPlanRepoStub struct {
	mu            sync.Mutex
	created       *ScheduledTestPlan
	due           []*ScheduledTestPlan
	afterRunCalls int
	listDue       func(context.Context, time.Time) ([]*ScheduledTestPlan, error)
	afterRun      func(context.Context, int64, time.Time, time.Time) error
}

func (r *scheduledTestPlanRepoStub) Create(_ context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	copied := *plan
	copied.ID = 1
	r.created = &copied
	return &copied, nil
}

func (r *scheduledTestPlanRepoStub) GetByID(_ context.Context, _ int64) (*ScheduledTestPlan, error) {
	return nil, nil
}

func (r *scheduledTestPlanRepoStub) ListByAccountID(_ context.Context, _ int64) ([]*ScheduledTestPlan, error) {
	return nil, nil
}

func (r *scheduledTestPlanRepoStub) ListDue(ctx context.Context, now time.Time) ([]*ScheduledTestPlan, error) {
	if r.listDue != nil {
		return r.listDue(ctx, now)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.due, nil
}

func (r *scheduledTestPlanRepoStub) Update(_ context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	copied := *plan
	return &copied, nil
}

func (r *scheduledTestPlanRepoStub) Delete(_ context.Context, _ int64) error { return nil }

func (r *scheduledTestPlanRepoStub) UpdateAfterRun(ctx context.Context, id int64, lastRun, nextRun time.Time) error {
	if r.afterRun != nil {
		if err := r.afterRun(ctx, id, lastRun, nextRun); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.afterRunCalls++
	return nil
}

type scheduledTestResultRepoStub struct {
	mu      sync.Mutex
	created []*ScheduledTestResult
	create  func(context.Context, *ScheduledTestResult) error
}

func (r *scheduledTestResultRepoStub) Create(ctx context.Context, result *ScheduledTestResult) (*ScheduledTestResult, error) {
	if r.create != nil {
		if err := r.create(ctx, result); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := *result
	copied.ID = int64(len(r.created) + 1)
	r.created = append(r.created, &copied)
	return &copied, nil
}

func (r *scheduledTestResultRepoStub) ListByPlanID(_ context.Context, _ int64, _ int) ([]*ScheduledTestResult, error) {
	return nil, nil
}

func (r *scheduledTestResultRepoStub) PruneOldResults(_ context.Context, _ int64, _ int) error {
	return nil
}

type codexDiagnosticRunnerStub struct {
	results []CodexDiagnosticModelResult
	err     error
	calls   int
}

func (s *codexDiagnosticRunnerStub) Run(
	_ context.Context,
	_ int64,
	_ int64,
	_ []string,
	_ func() (ModelTraceChallenge, error),
) ([]CodexDiagnosticModelResult, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.results, nil
}

func newScheduledTestRunnerForTest(runner CodexDiagnosticRunner) (*ScheduledTestRunnerService, *scheduledTestPlanRepoStub, *scheduledTestResultRepoStub) {
	planRepo := &scheduledTestPlanRepoStub{}
	resultRepo := &scheduledTestResultRepoStub{}
	scheduledSvc := NewScheduledTestService(planRepo, resultRepo)
	return NewScheduledTestRunnerService(planRepo, scheduledSvc, nil, nil, &config.Config{}, runner), planRepo, resultRepo
}

func TestScheduledTestService_CreatePlanNormalizesDiagnosticPlan(t *testing.T) {
	planRepo := &scheduledTestPlanRepoStub{}
	resultRepo := &scheduledTestResultRepoStub{}
	svc := NewScheduledTestService(planRepo, resultRepo)

	apiKeyID := int64(42)
	plan, err := svc.CreatePlan(context.Background(), &ScheduledTestPlan{
		AccountID:      7,
		TestType:       ScheduledTestTypeCodexDiagnostic,
		Models:         []string{"gpt-6-astra", " gpt-6-astra ", "gpt-5.4", ""},
		APIKeyID:       &apiKeyID,
		CronExpression: "*/30 * * * *",
	})
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.Equal(t, ScheduledTestTypeCodexDiagnostic, plan.TestType)
	require.Equal(t, []string{"gpt-6-astra", "gpt-5.4"}, plan.Models)
	require.Equal(t, "", plan.ModelID)
	require.False(t, plan.AutoRecover)
	require.Equal(t, 50, plan.MaxResults)
	require.NotNil(t, plan.NextRunAt)
	require.NotNil(t, planRepo.created)
}

func TestScheduledTestService_CreatePlanRejectsInvalidDiagnosticPlan(t *testing.T) {
	svc := NewScheduledTestService(&scheduledTestPlanRepoStub{}, &scheduledTestResultRepoStub{})

	_, err := svc.CreatePlan(context.Background(), &ScheduledTestPlan{
		AccountID:      7,
		TestType:       ScheduledTestTypeCodexDiagnostic,
		Models:         []string{"gpt-6-astra"},
		CronExpression: "*/30 * * * *",
	})
	require.Error(t, err, "diagnostic plans require an API key")

	apiKeyID := int64(42)
	_, err = svc.CreatePlan(context.Background(), &ScheduledTestPlan{
		AccountID:      7,
		TestType:       ScheduledTestTypeCodexDiagnostic,
		APIKeyID:       &apiKeyID,
		CronExpression: "*/30 * * * *",
	})
	require.Error(t, err, "diagnostic plans require at least one model")

	_, err = svc.CreatePlan(context.Background(), &ScheduledTestPlan{
		AccountID:      7,
		TestType:       "unknown",
		CronExpression: "*/30 * * * *",
	})
	require.Error(t, err)
}

func TestScheduledTestRunner_RunsDiagnosticPlanAndStoresPerModelResults(t *testing.T) {
	probability := 0.93
	firstStart := time.Now().Add(-time.Minute)
	firstFinish := firstStart.Add(120 * time.Millisecond)
	secondStart := firstFinish.Add(time.Second)
	secondFinish := secondStart.Add(80 * time.Millisecond)
	runnerStub := &codexDiagnosticRunnerStub{results: []CodexDiagnosticModelResult{
		{
			Model:          "gpt-6-astra",
			Status:         ScheduledDiagnosticStatusNormal,
			PredictedModel: "gpt-6-astra",
			Probability:    &probability,
			HTTPStatus:     200,
			ModelReply:     "12345",
			LatencyMs:      120,
			StartedAt:      firstStart,
			FinishedAt:     firstFinish,
		},
		{
			Model:            "gpt-5.4",
			Status:           ScheduledDiagnosticStatusFailed,
			Reason:           "gateway_request_failed",
			GatewayErrorCode: "MODEL_NOT_ALLOWED",
			HTTPStatus:       403,
			LatencyMs:        80,
			StartedAt:        secondStart,
			FinishedAt:       secondFinish,
		},
	}}
	runner, planRepo, resultRepo := newScheduledTestRunnerForTest(runnerStub)

	apiKeyID := int64(42)
	plan := &ScheduledTestPlan{
		ID:             9,
		AccountID:      7,
		TestType:       ScheduledTestTypeCodexDiagnostic,
		Models:         []string{"gpt-6-astra", "gpt-5.4"},
		APIKeyID:       &apiKeyID,
		CronExpression: "*/30 * * * *",
		MaxResults:     20,
	}
	runner.runOnePlan(context.Background(), plan)

	require.Equal(t, 1, runnerStub.calls)
	require.Len(t, resultRepo.created, 2)
	first := resultRepo.created[0]
	require.Equal(t, "gpt-6-astra", first.ModelID)
	require.Equal(t, ScheduledDiagnosticStatusNormal, first.Status)
	require.Equal(t, "gpt-6-astra", first.PredictedModel)
	require.NotNil(t, first.Probability)
	require.InDelta(t, 0.93, *first.Probability, 1e-9)
	require.Equal(t, 200, first.HTTPStatus)
	require.Equal(t, firstStart, first.StartedAt)
	require.Equal(t, firstFinish, first.FinishedAt)

	second := resultRepo.created[1]
	require.Equal(t, "gpt-5.4", second.ModelID)
	require.Equal(t, ScheduledDiagnosticStatusFailed, second.Status)
	require.Equal(t, "gateway_request_failed", second.Reason)
	require.Contains(t, second.ErrorMessage, "MODEL_NOT_ALLOWED")
	require.Equal(t, 403, second.HTTPStatus)
	require.Equal(t, secondStart, second.StartedAt)
	require.Equal(t, secondFinish, second.FinishedAt)
	require.True(t, first.FinishedAt.Before(second.StartedAt), "serial probes must keep their actual execution times")

	require.Equal(t, 1, planRepo.afterRunCalls)
}

type scheduledTestDiagnosticFunc func(context.Context, int64, int64, []string, func() (ModelTraceChallenge, error)) ([]CodexDiagnosticModelResult, error)

func (f scheduledTestDiagnosticFunc) Run(ctx context.Context, accountID, keyID int64, models []string, challenge func() (ModelTraceChallenge, error)) ([]CodexDiagnosticModelResult, error) {
	return f(ctx, accountID, keyID, models, challenge)
}

func scheduledTestDiagnosticPlan(id int64) *ScheduledTestPlan {
	keyID := int64(42)
	return &ScheduledTestPlan{
		ID: id, AccountID: 7, APIKeyID: &keyID,
		TestType: ScheduledTestTypeCodexDiagnostic, Models: []string{"gpt-5.4"},
		CronExpression: "* * * * *", MaxResults: 20,
	}
}

func scheduledTestWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled test runner did not finish")
	}
}

func TestScheduledTestRunner_OverlappingTicksRunPlanOnlyOnce(t *testing.T) {
	release := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	started := make(chan struct{}, 2)
	var calls atomic.Int32
	runner, plans, _ := newScheduledTestRunnerForTest(scheduledTestDiagnosticFunc(func(ctx context.Context, _, _ int64, _ []string, _ func() (ModelTraceChallenge, error)) ([]CodexDiagnosticModelResult, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return nil, nil
	}))
	plans.due = []*ScheduledTestPlan{scheduledTestDiagnosticPlan(1)}
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); runner.runScheduledBatch(context.Background()) }()
	scheduledTestWait(t, started)

	secondDone := make(chan struct{})
	go func() { defer close(secondDone); runner.runScheduledBatch(context.Background()) }()
	select {
	case <-secondDone:
	case <-started:
		t.Error("overlapping tick executed the same in-flight plan twice")
	case <-time.After(5 * time.Second):
		t.Error("overlapping tick waited for an already-running plan")
	}
	unblock.Do(func() { close(release) })
	scheduledTestWait(t, firstDone)
	scheduledTestWait(t, secondDone)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, 1, plans.afterRunCalls)
}

func TestScheduledTestRunner_WorkerLimitSharedAcrossBatches(t *testing.T) {
	release := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	started := make(chan struct{}, scheduledTestDefaultMaxWorkers+1)
	var calls atomic.Int32
	runner, plans, _ := newScheduledTestRunnerForTest(scheduledTestDiagnosticFunc(func(ctx context.Context, _, _ int64, _ []string, _ func() (ModelTraceChallenge, error)) ([]CodexDiagnosticModelResult, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return nil, nil
	}))
	firstPlans := make([]*ScheduledTestPlan, scheduledTestDefaultMaxWorkers)
	for i := range firstPlans {
		firstPlans[i] = scheduledTestDiagnosticPlan(int64(i + 1))
	}
	var scans atomic.Int32
	scannedSecond := make(chan struct{})
	plans.listDue = func(context.Context, time.Time) ([]*ScheduledTestPlan, error) {
		if scans.Add(1) == 1 {
			return firstPlans, nil
		}
		close(scannedSecond)
		return []*ScheduledTestPlan{scheduledTestDiagnosticPlan(99)}, nil
	}
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); runner.runScheduledBatch(context.Background()) }()
	for range scheduledTestDefaultMaxWorkers {
		scheduledTestWait(t, started)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secondDone := make(chan struct{})
	go func() { defer close(secondDone); runner.runScheduledBatch(ctx) }()
	scheduledTestWait(t, scannedSecond)
	select {
	case <-started:
		t.Error("a new batch bypassed the instance-wide worker limit")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	// A queued plan must stop waiting promptly, without a probe or advance.
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Error("canceled batch kept waiting for a worker")
	}
	unblock.Do(func() { close(release) })
	scheduledTestWait(t, firstDone)
	scheduledTestWait(t, secondDone)
	require.Equal(t, int32(scheduledTestDefaultMaxWorkers), calls.Load())
	require.Equal(t, scheduledTestDefaultMaxWorkers, plans.afterRunCalls)
	require.Empty(t, runner.inflight, "canceled queued plans and completed plans must release their claims")
	require.Empty(t, runner.workers, "completed plans must return worker slots")
}

func TestScheduledTestRunner_ConcurrentScansDoNotDuplicateWork(t *testing.T) {
	scanned := make(chan struct{})
	releaseScan := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(releaseScan) })
	var scans atomic.Int32
	diagnostic := &codexDiagnosticRunnerStub{}
	runner, plans, _ := newScheduledTestRunnerForTest(diagnostic)
	plans.listDue = func(context.Context, time.Time) ([]*ScheduledTestPlan, error) {
		if scans.Add(1) == 1 {
			close(scanned)
		}
		<-releaseScan
		plan := scheduledTestDiagnosticPlan(1)
		return []*ScheduledTestPlan{plan, plan}, nil
	}
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); runner.runScheduledBatch(context.Background()) }()
	scheduledTestWait(t, scanned)
	secondDone := make(chan struct{})
	go func() { defer close(secondDone); runner.runScheduledBatch(context.Background()) }()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Error("a tick queued a second ListDue scan")
	}
	unblock.Do(func() { close(releaseScan) })
	scheduledTestWait(t, firstDone)
	scheduledTestWait(t, secondDone)
	require.Equal(t, int32(1), scans.Load())
	require.Equal(t, 1, diagnostic.calls, "duplicate plan IDs from one scan must also be claimed once")
}

func TestScheduledTestRunner_StaleScanCannotReclaimCompletingPlan(t *testing.T) {
	started := make(chan struct{})
	releaseRun := make(chan struct{})
	var unblockRun sync.Once
	defer unblockRun.Do(func() { close(releaseRun) })
	var calls atomic.Int32
	runner, plans, _ := newScheduledTestRunnerForTest(scheduledTestDiagnosticFunc(func(context.Context, int64, int64, []string, func() (ModelTraceChallenge, error)) ([]CodexDiagnosticModelResult, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-releaseRun
		return nil, nil
	}))
	scannedSecond := make(chan struct{})
	releaseScan := make(chan struct{})
	var unblockScan sync.Once
	defer unblockScan.Do(func() { close(releaseScan) })
	var scans atomic.Int32
	plans.listDue = func(context.Context, time.Time) ([]*ScheduledTestPlan, error) {
		if scans.Add(1) == 2 {
			close(scannedSecond)
			<-releaseScan
		}
		return []*ScheduledTestPlan{scheduledTestDiagnosticPlan(1)}, nil
	}
	advanced := make(chan struct{}, 2)
	plans.afterRun = func(context.Context, int64, time.Time, time.Time) error {
		advanced <- struct{}{}
		return nil
	}
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); runner.runScheduledBatch(context.Background()) }()
	scheduledTestWait(t, started)
	secondDone := make(chan struct{})
	go func() { defer close(secondDone); runner.runScheduledBatch(context.Background()) }()
	scheduledTestWait(t, scannedSecond)
	unblockRun.Do(func() { close(releaseRun) })
	scheduledTestWait(t, advanced)
	unblockScan.Do(func() { close(releaseScan) })
	scheduledTestWait(t, firstDone)
	scheduledTestWait(t, secondDone)
	require.Equal(t, int32(1), calls.Load(), "stale due snapshot must not execute the just-completed plan again")
	require.Equal(t, 1, plans.afterRunCalls)

	// A subsequent genuinely-due scan can claim the plan after completion.
	runner.runScheduledBatch(context.Background())
	require.Equal(t, int32(2), calls.Load())
}

func TestScheduledTestRunner_CanceledDiagnosticPreservesPartialResultsAndAdvances(t *testing.T) {
	for _, returnErr := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial_only", true: "partial_and_error"}[returnErr], func(t *testing.T) {
			type key struct{}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "trace"))
			defer cancel()
			runner, plans, results := newScheduledTestRunnerForTest(scheduledTestDiagnosticFunc(func(runCtx context.Context, _, _ int64, _ []string, _ func() (ModelTraceChallenge, error)) ([]CodexDiagnosticModelResult, error) {
				cancel()
				<-runCtx.Done()
				var err error
				if returnErr {
					err = runCtx.Err()
				}
				return []CodexDiagnosticModelResult{{Model: "gpt-5.4", Status: ScheduledDiagnosticStatusNormal, ModelReply: "completed"}}, err
			}))
			checkFinalization := func(finalCtx context.Context) error {
				require.NoError(t, finalCtx.Err(), "finalization must survive execution cancellation")
				require.Equal(t, "trace", finalCtx.Value(key{}), "context values must survive")
				deadline, ok := finalCtx.Deadline()
				require.True(t, ok, "finalization must remain bounded")
				require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
				return finalCtx.Err()
			}
			results.create = func(finalCtx context.Context, _ *ScheduledTestResult) error { return checkFinalization(finalCtx) }
			plans.afterRun = func(finalCtx context.Context, _ int64, _, next time.Time) error {
				require.True(t, next.After(time.Now()))
				return checkFinalization(finalCtx)
			}
			plan := scheduledTestDiagnosticPlan(1)
			plan.Models = []string{"gpt-5.4", "gpt-6-astra"}
			runner.runOnePlan(ctx, plan)
			require.Len(t, results.created, 1, "do not replace partial results with failures for unprobed models")
			require.Equal(t, ScheduledDiagnosticStatusNormal, results.created[0].Status)
			require.Equal(t, "completed", results.created[0].ResponseText)
			require.Equal(t, 1, plans.afterRunCalls)
		})
	}
}

func TestScheduledTestRunner_DiagnosticErrorStoresFailurePerModel(t *testing.T) {
	runnerStub := &codexDiagnosticRunnerStub{err: errors.New("executor unavailable")}
	runner, planRepo, resultRepo := newScheduledTestRunnerForTest(runnerStub)

	apiKeyID := int64(42)
	plan := &ScheduledTestPlan{
		ID:             11,
		AccountID:      7,
		TestType:       ScheduledTestTypeCodexDiagnostic,
		Models:         []string{"gpt-6-astra", "gpt-5.4"},
		APIKeyID:       &apiKeyID,
		CronExpression: "*/30 * * * *",
		MaxResults:     20,
	}
	runner.runOnePlan(context.Background(), plan)

	require.Len(t, resultRepo.created, 2)
	for _, result := range resultRepo.created {
		require.Equal(t, ScheduledDiagnosticStatusFailed, result.Status)
		require.Equal(t, "diagnostic_unavailable", result.Reason)
		require.Contains(t, result.ErrorMessage, "executor unavailable")
	}
	require.Equal(t, 1, planRepo.afterRunCalls)
}
