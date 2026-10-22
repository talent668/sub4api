package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var scheduledTestCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// maxScheduledDiagnosticModels bounds how many models one diagnostic plan may
// probe per run: each probe is a real billed gateway request.
const maxScheduledDiagnosticModels = 16

// ScheduledTestService provides CRUD operations for scheduled test plans and results.
type ScheduledTestService struct {
	planRepo   ScheduledTestPlanRepository
	resultRepo ScheduledTestResultRepository
}

// NewScheduledTestService creates a new ScheduledTestService.
func NewScheduledTestService(
	planRepo ScheduledTestPlanRepository,
	resultRepo ScheduledTestResultRepository,
) *ScheduledTestService {
	return &ScheduledTestService{
		planRepo:   planRepo,
		resultRepo: resultRepo,
	}
}

// CreatePlan validates the cron expression, computes next_run_at, and persists the plan.
func (s *ScheduledTestService) CreatePlan(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	if err := normalizeScheduledTestPlan(plan); err != nil {
		return nil, err
	}
	nextRun, err := computeNextRun(plan.CronExpression, time.Now())
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	plan.NextRunAt = &nextRun

	if plan.MaxResults <= 0 {
		plan.MaxResults = 50
	}

	return s.planRepo.Create(ctx, plan)
}

// GetPlan retrieves a plan by ID.
func (s *ScheduledTestService) GetPlan(ctx context.Context, id int64) (*ScheduledTestPlan, error) {
	return s.planRepo.GetByID(ctx, id)
}

// ListPlansByAccount returns all plans for a given account.
func (s *ScheduledTestService) ListPlansByAccount(ctx context.Context, accountID int64) ([]*ScheduledTestPlan, error) {
	return s.planRepo.ListByAccountID(ctx, accountID)
}

// UpdatePlan validates cron and updates the plan.
func (s *ScheduledTestService) UpdatePlan(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	// A deleted billing key is SET NULL by the FK. Keep existing plans
	// disableable without allowing creation or activation of an invalid plan.
	allowMissingKey := plan != nil && plan.ID > 0 && !plan.Enabled
	if err := normalizeScheduledTestPlanWithOptions(plan, allowMissingKey); err != nil {
		return nil, err
	}
	nextRun, err := computeNextRun(plan.CronExpression, time.Now())
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	plan.NextRunAt = &nextRun

	return s.planRepo.Update(ctx, plan)
}

// DeletePlan removes a plan and its results (via CASCADE).
func (s *ScheduledTestService) DeletePlan(ctx context.Context, id int64) error {
	return s.planRepo.Delete(ctx, id)
}

// ListResults returns the most recent results for a plan.
func (s *ScheduledTestService) ListResults(ctx context.Context, planID int64, limit int) ([]*ScheduledTestResult, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.resultRepo.ListByPlanID(ctx, planID, limit)
}

// SaveResult inserts a result and prunes old entries beyond maxResults.
func (s *ScheduledTestService) SaveResult(ctx context.Context, planID int64, maxResults int, result *ScheduledTestResult) error {
	result.PlanID = planID
	if _, err := s.resultRepo.Create(ctx, result); err != nil {
		return err
	}
	return s.resultRepo.PruneOldResults(ctx, planID, maxResults)
}

func computeNextRun(cronExpr string, from time.Time) (time.Time, error) {
	sched, err := scheduledTestCronParser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return sched.Next(from), nil
}

// normalizeScheduledTestPlan validates and canonicalizes a plan before it is
// persisted. Connectivity tests keep the legacy single-model contract while
// Codex diagnostic plans carry a billing API key and a model list.
func normalizeScheduledTestPlan(plan *ScheduledTestPlan) error {
	return normalizeScheduledTestPlanWithOptions(plan, false)
}

func normalizeScheduledTestPlanWithOptions(plan *ScheduledTestPlan, allowMissingKey bool) error {
	if plan == nil {
		return errors.New("plan is nil")
	}
	plan.TestType = strings.ToLower(strings.TrimSpace(plan.TestType))
	switch plan.TestType {
	case "", ScheduledTestTypeAccountTest:
		plan.TestType = ScheduledTestTypeAccountTest
		plan.ModelID = strings.TrimSpace(plan.ModelID)
		if plan.ModelID == "" {
			return errors.New("model_id is required for account test plans")
		}
		plan.Models = nil
		plan.APIKeyID = nil
	case ScheduledTestTypeCodexDiagnostic:
		if (plan.APIKeyID == nil || *plan.APIKeyID <= 0) && !allowMissingKey {
			return errors.New("api_key_id is required for codex diagnostic plans")
		}
		models := make([]string, 0, len(plan.Models))
		seen := make(map[string]bool, len(plan.Models))
		for _, model := range plan.Models {
			model = strings.TrimSpace(model)
			if model == "" || seen[model] {
				continue
			}
			seen[model] = true
			models = append(models, model)
		}
		if len(models) == 0 {
			return errors.New("at least one model is required for codex diagnostic plans")
		}
		if len(models) > maxScheduledDiagnosticModels {
			return fmt.Errorf("too many models for one codex diagnostic plan (max %d)", maxScheduledDiagnosticModels)
		}
		plan.Models = models
		plan.ModelID = ""
		// Diagnostics are informational; they must never mutate account state.
		plan.AutoRecover = false
	default:
		return fmt.Errorf("unsupported test_type %q", plan.TestType)
	}
	if plan.CronExpression == "" {
		return errors.New("cron_expression is required")
	}
	if plan.MaxResults <= 0 {
		plan.MaxResults = 50
	}
	if plan.MaxResults > 500 {
		plan.MaxResults = 500
	}
	return nil
}
