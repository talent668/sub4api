package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
)

type scheduledTestPlanRepoStub struct {
	created       *ScheduledTestPlan
	due           []*ScheduledTestPlan
	afterRunCalls int
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

func (r *scheduledTestPlanRepoStub) ListDue(_ context.Context, _ time.Time) ([]*ScheduledTestPlan, error) {
	return r.due, nil
}

func (r *scheduledTestPlanRepoStub) Update(_ context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	copied := *plan
	return &copied, nil
}

func (r *scheduledTestPlanRepoStub) Delete(_ context.Context, _ int64) error { return nil }

func (r *scheduledTestPlanRepoStub) UpdateAfterRun(_ context.Context, _ int64, _ time.Time, _ time.Time) error {
	r.afterRunCalls++
	return nil
}

type scheduledTestResultRepoStub struct {
	created []*ScheduledTestResult
}

func (r *scheduledTestResultRepoStub) Create(_ context.Context, result *ScheduledTestResult) (*ScheduledTestResult, error) {
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
	runnerStub := &codexDiagnosticRunnerStub{results: []CodexDiagnosticModelResult{
		{
			Model:          "gpt-6-astra",
			Status:         ScheduledDiagnosticStatusNormal,
			PredictedModel: "gpt-6-astra",
			Probability:    &probability,
			HTTPStatus:     200,
			ModelReply:     "12345",
			LatencyMs:      120,
		},
		{
			Model:            "gpt-5.4",
			Status:           ScheduledDiagnosticStatusFailed,
			Reason:           "gateway_request_failed",
			GatewayErrorCode: "MODEL_NOT_ALLOWED",
			HTTPStatus:       403,
			LatencyMs:        80,
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

	second := resultRepo.created[1]
	require.Equal(t, "gpt-5.4", second.ModelID)
	require.Equal(t, ScheduledDiagnosticStatusFailed, second.Status)
	require.Equal(t, "gateway_request_failed", second.Reason)
	require.Contains(t, second.ErrorMessage, "MODEL_NOT_ALLOWED")
	require.Equal(t, 403, second.HTTPStatus)

	require.Equal(t, 1, planRepo.afterRunCalls)
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
