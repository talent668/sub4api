package service

import (
	"context"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
)

const (
	scheduledTestDefaultMaxWorkers = 10
	scheduledTestFinalizeTimeout   = 15 * time.Second
)

// ScheduledTestRunnerService periodically scans due test plans and executes them.
type ScheduledTestRunnerService struct {
	planRepo       ScheduledTestPlanRepository
	scheduledSvc   *ScheduledTestService
	accountTestSvc *AccountTestService
	rateLimitSvc   *RateLimitService
	diagnostic     CodexDiagnosticRunner
	cfg            *config.Config

	cron      *cron.Cron
	startOnce sync.Once
	stopOnce  sync.Once

	// Serialize scans and claims with completion so a stale ListDue snapshot
	// cannot reclaim a plan just as its previous execution finishes.
	scanMu   sync.Mutex
	inflight map[int64]struct{}
	workers  chan struct{}
}

// NewScheduledTestRunnerService creates a new runner.
func NewScheduledTestRunnerService(
	planRepo ScheduledTestPlanRepository,
	scheduledSvc *ScheduledTestService,
	accountTestSvc *AccountTestService,
	rateLimitSvc *RateLimitService,
	cfg *config.Config,
	diagnostic CodexDiagnosticRunner,
) *ScheduledTestRunnerService {
	return &ScheduledTestRunnerService{
		planRepo:       planRepo,
		scheduledSvc:   scheduledSvc,
		accountTestSvc: accountTestSvc,
		rateLimitSvc:   rateLimitSvc,
		diagnostic:     diagnostic,
		cfg:            cfg,
	}
}

// Start begins the cron ticker (every minute).
func (s *ScheduledTestRunnerService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		loc := time.Local
		if s.cfg != nil {
			if parsed, err := time.LoadLocation(s.cfg.Timezone); err == nil && parsed != nil {
				loc = parsed
			}
		}

		c := cron.New(cron.WithParser(scheduledTestCronParser), cron.WithLocation(loc))
		_, err := c.AddFunc("* * * * *", func() { s.runScheduled() })
		if err != nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] not started (invalid schedule): %v", err)
			return
		}
		s.cron = c
		s.cron.Start()
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] started (tick=every minute)")
	})
}

// Stop gracefully shuts down the cron scheduler.
func (s *ScheduledTestRunnerService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cron != nil {
			ctx := s.cron.Stop()
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
				logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] cron stop timed out")
			}
		}
	})
}

func (s *ScheduledTestRunnerService) runScheduled() {
	// Delay 10s so execution lands at ~:10 of each minute instead of :00.
	time.Sleep(10 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	s.runScheduledBatch(ctx)
}

// runScheduledBatch scans and executes due plans without the cron tick delay.
func (s *ScheduledTestRunnerService) runScheduledBatch(ctx context.Context) {
	// Overlapping ticks need not queue another scan. Keep the lock until all
	// due plans are claimed; completion uses this same lock before releasing a
	// claim, preventing a stale scan from re-running a just-finished plan.
	if !s.scanMu.TryLock() {
		return
	}
	if ctx.Err() != nil {
		s.scanMu.Unlock()
		return
	}
	plans, err := s.planRepo.ListDue(ctx, time.Now())
	if err != nil {
		s.scanMu.Unlock()
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] ListDue error: %v", err)
		return
	}
	if s.inflight == nil {
		s.inflight = make(map[int64]struct{})
	}
	if s.workers == nil {
		s.workers = make(chan struct{}, scheduledTestDefaultMaxWorkers)
	}
	workers := s.workers
	claimed := make([]*ScheduledTestPlan, 0, len(plans))
	for _, plan := range plans {
		if plan == nil {
			continue
		}
		if _, running := s.inflight[plan.ID]; running {
			continue
		}
		s.inflight[plan.ID] = struct{}{}
		claimed = append(claimed, plan)
	}
	s.scanMu.Unlock()
	if len(claimed) == 0 {
		return
	}

	logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] found %d due plans", len(claimed))
	var wg sync.WaitGroup
	launched := 0
dispatch:
	for _, plan := range claimed {
		select {
		case workers <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		// A ready worker and cancellation can race in the select above.
		if ctx.Err() != nil {
			<-workers
			break
		}
		launched++
		wg.Add(1)
		go func(p *ScheduledTestPlan) {
			defer wg.Done()
			defer s.releasePlan(p.ID)
			defer func() { <-workers }()
			s.runOnePlan(ctx, p)
		}(plan)
	}
	// Plans that never obtained a worker remain due for a later tick.
	for _, plan := range claimed[launched:] {
		s.releasePlan(plan.ID)
	}
	wg.Wait()
}

func (s *ScheduledTestRunnerService) releasePlan(id int64) {
	s.scanMu.Lock()
	delete(s.inflight, id)
	s.scanMu.Unlock()
}

func (s *ScheduledTestRunnerService) runOnePlan(ctx context.Context, plan *ScheduledTestPlan) {
	if plan.TestType == ScheduledTestTypeCodexDiagnostic {
		s.runDiagnosticPlan(ctx, plan)
		s.advancePlan(ctx, plan)
		return
	}

	result, err := s.accountTestSvc.RunTestBackground(ctx, plan.AccountID, plan.ModelID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d RunTestBackground error: %v", plan.ID, err)
		return
	}

	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledTestFinalizeTimeout)
	if err := s.scheduledSvc.SaveResult(finalCtx, plan.ID, plan.MaxResults, result); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d SaveResult error: %v", plan.ID, err)
	}
	cancel()

	// Auto-recover account if test succeeded and auto_recover is enabled.
	if result.Status == "success" && plan.AutoRecover {
		s.tryRecoverAccount(ctx, plan.AccountID, plan.ID)
	}

	s.advancePlan(ctx, plan)
}

// runDiagnosticPlan executes one Codex degradation-diagnostic plan: each model
// is probed serially through the normal gateway and stored as its own result
// row. Diagnostics never mutate account state.
func (s *ScheduledTestRunnerService) runDiagnosticPlan(ctx context.Context, plan *ScheduledTestPlan) {
	if s.diagnostic == nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d skipped: codex diagnostic executor unavailable", plan.ID)
		return
	}
	startedAt := time.Now()
	if plan.APIKeyID == nil || *plan.APIKeyID <= 0 || len(plan.Models) == 0 {
		finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledTestFinalizeTimeout)
		defer cancel()
		s.saveDiagnosticFailure(finalCtx, plan, "", "invalid_plan", "diagnostic plan is missing api_key_id or models", startedAt)
		return
	}
	results, err := s.diagnostic.Run(ctx, plan.AccountID, *plan.APIKeyID, plan.Models, nil)
	// Probes keep the batch's execution budget, but completed work must survive
	// its cancellation. Keep values (tracing, etc.) and bound all result writes.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledTestFinalizeTimeout)
	defer cancel()
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d diagnostic error: %v", plan.ID, err)
		if len(results) == 0 {
			for _, model := range plan.Models {
				s.saveDiagnosticFailure(finalCtx, plan, model, "diagnostic_unavailable", err.Error(), startedAt)
			}
			return
		}
		// A partial result plus cancellation is not a setup failure. Preserve
		// returned probes and don't invent failures for unprobed models.
	}
	for _, modelResult := range results {
		finishedAt := modelResult.FinishedAt
		if finishedAt.IsZero() {
			finishedAt = time.Now()
		}
		started := modelResult.StartedAt
		if started.IsZero() {
			started = finishedAt.Add(-time.Duration(modelResult.LatencyMs) * time.Millisecond)
		}
		status := modelResult.Status
		if status == "" {
			status = ScheduledDiagnosticStatusFailed
		}
		errorMessage := ""
		if status == ScheduledDiagnosticStatusFailed || status == ScheduledDiagnosticStatusUncertain {
			errorMessage = modelResult.Reason
			if modelResult.GatewayErrorCode != "" {
				if errorMessage != "" {
					errorMessage += ": "
				}
				errorMessage += modelResult.GatewayErrorCode
			}
		}
		entry := &ScheduledTestResult{
			ModelID:        modelResult.Model,
			Status:         status,
			ResponseText:   modelResult.ModelReply,
			ErrorMessage:   errorMessage,
			PredictedModel: modelResult.PredictedModel,
			Probability:    modelResult.Probability,
			Reason:         modelResult.Reason,
			HTTPStatus:     modelResult.HTTPStatus,
			LatencyMs:      modelResult.LatencyMs,
			StartedAt:      started,
			FinishedAt:     finishedAt,
		}
		if saveErr := s.scheduledSvc.SaveResult(finalCtx, plan.ID, plan.MaxResults, entry); saveErr != nil {
			logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d SaveResult error: %v", plan.ID, saveErr)
		}
	}
}

func (s *ScheduledTestRunnerService) saveDiagnosticFailure(ctx context.Context, plan *ScheduledTestPlan, model, reason, message string, startedAt time.Time) {
	finishedAt := time.Now()
	entry := &ScheduledTestResult{
		ModelID:      model,
		Status:       ScheduledDiagnosticStatusFailed,
		ErrorMessage: message,
		Reason:       reason,
		StartedAt:    startedAt,
		FinishedAt:   finishedAt,
		LatencyMs:    finishedAt.Sub(startedAt).Milliseconds(),
	}
	if err := s.scheduledSvc.SaveResult(ctx, plan.ID, plan.MaxResults, entry); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d SaveResult error: %v", plan.ID, err)
	}
}

// advancePlan schedules the next run for a plan.
func (s *ScheduledTestRunnerService) advancePlan(ctx context.Context, plan *ScheduledTestPlan) {
	// Give schedule advancement its own bounded window even if probe execution
	// or result persistence exhausted their deadlines.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scheduledTestFinalizeTimeout)
	defer cancel()
	now := time.Now()
	nextRun, err := computeNextRun(plan.CronExpression, now)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d computeNextRun error: %v", plan.ID, err)
		return
	}

	if err := s.planRepo.UpdateAfterRun(finalCtx, plan.ID, now, nextRun); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d UpdateAfterRun error: %v", plan.ID, err)
	}
}

// tryRecoverAccount attempts to recover an account from recoverable runtime state.
func (s *ScheduledTestRunnerService) tryRecoverAccount(ctx context.Context, accountID int64, planID int64) {
	if s.rateLimitSvc == nil {
		return
	}

	recovery, err := s.rateLimitSvc.RecoverAccountAfterSuccessfulTest(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover failed: %v", planID, err)
		return
	}
	if recovery == nil {
		return
	}

	if recovery.ClearedError {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d recovered from error status", planID, accountID)
	}
	if recovery.ClearedRateLimit {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d cleared rate-limit/runtime state", planID, accountID)
	}
}
