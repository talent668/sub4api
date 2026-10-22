package service

import (
	"context"
	"sync"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/robfig/cron/v3"
)

const scheduledTestDefaultMaxWorkers = 10

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

	now := time.Now()
	plans, err := s.planRepo.ListDue(ctx, now)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] ListDue error: %v", err)
		return
	}
	if len(plans) == 0 {
		return
	}

	logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] found %d due plans", len(plans))

	sem := make(chan struct{}, scheduledTestDefaultMaxWorkers)
	var wg sync.WaitGroup

	for _, plan := range plans {
		sem <- struct{}{}
		wg.Add(1)
		go func(p *ScheduledTestPlan) {
			defer wg.Done()
			defer func() { <-sem }()
			s.runOnePlan(ctx, p)
		}(plan)
	}

	wg.Wait()
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

	if err := s.scheduledSvc.SaveResult(ctx, plan.ID, plan.MaxResults, result); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d SaveResult error: %v", plan.ID, err)
	}

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
		s.saveDiagnosticFailure(ctx, plan, "", "invalid_plan", "diagnostic plan is missing api_key_id or models", startedAt)
		return
	}
	results, err := s.diagnostic.Run(ctx, plan.AccountID, *plan.APIKeyID, plan.Models, nil)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d diagnostic error: %v", plan.ID, err)
		for _, model := range plan.Models {
			s.saveDiagnosticFailure(ctx, plan, model, "diagnostic_unavailable", err.Error(), startedAt)
		}
		return
	}
	for _, modelResult := range results {
		finishedAt := time.Now()
		started := finishedAt.Add(-time.Duration(modelResult.LatencyMs) * time.Millisecond)
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
		if saveErr := s.scheduledSvc.SaveResult(ctx, plan.ID, plan.MaxResults, entry); saveErr != nil {
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
	nextRun, err := computeNextRun(plan.CronExpression, time.Now())
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d computeNextRun error: %v", plan.ID, err)
		return
	}

	if err := s.planRepo.UpdateAfterRun(ctx, plan.ID, time.Now(), nextRun); err != nil {
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
