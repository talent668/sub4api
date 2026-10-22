package service

import (
	"context"
	"time"
)

// Scheduled test plan types.
const (
	// ScheduledTestTypeAccountTest is the legacy connectivity/latency test.
	ScheduledTestTypeAccountTest = "account_test"
	// ScheduledTestTypeCodexDiagnostic runs the Codex model degradation
	// diagnostic (ModelTrace fingerprint) through the normal gateway.
	ScheduledTestTypeCodexDiagnostic = "codex_diagnostic"
)

// Codex diagnostic result statuses. Account tests keep using success/failed.
const (
	ScheduledDiagnosticStatusNormal    = "normal"
	ScheduledDiagnosticStatusDegraded  = "degraded"
	ScheduledDiagnosticStatusUncertain = "uncertain"
	ScheduledDiagnosticStatusFailed    = "failed"
)

// ScheduledTestPlan represents a scheduled test plan domain model.
type ScheduledTestPlan struct {
	ID             int64      `json:"id"`
	AccountID      int64      `json:"account_id"`
	TestType       string     `json:"test_type"`
	ModelID        string     `json:"model_id"`
	Models         []string   `json:"models"`
	APIKeyID       *int64     `json:"api_key_id"`
	CronExpression string     `json:"cron_expression"`
	Enabled        bool       `json:"enabled"`
	MaxResults     int        `json:"max_results"`
	AutoRecover    bool       `json:"auto_recover"`
	LastRunAt      *time.Time `json:"last_run_at"`
	NextRunAt      *time.Time `json:"next_run_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ScheduledTestResult represents a single test execution result.
type ScheduledTestResult struct {
	ID             int64     `json:"id"`
	PlanID         int64     `json:"plan_id"`
	ModelID        string    `json:"model_id"`
	Status         string    `json:"status"`
	ResponseText   string    `json:"response_text"`
	ErrorMessage   string    `json:"error_message"`
	PredictedModel string    `json:"predicted_model"`
	Probability    *float64  `json:"probability"`
	Reason         string    `json:"reason"`
	HTTPStatus     int       `json:"http_status"`
	LatencyMs      int64     `json:"latency_ms"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// CodexDiagnosticRunner executes the Codex degradation diagnostic for a
// scheduled plan. Implemented by CodexDiagnosticExecutor.
type CodexDiagnosticRunner interface {
	Run(
		ctx context.Context,
		accountID int64,
		apiKeyID int64,
		models []string,
		generateChallenge func() (ModelTraceChallenge, error),
	) ([]CodexDiagnosticModelResult, error)
}

// ScheduledTestPlanRepository defines the data access interface for test plans.
type ScheduledTestPlanRepository interface {
	Create(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error)
	GetByID(ctx context.Context, id int64) (*ScheduledTestPlan, error)
	ListByAccountID(ctx context.Context, accountID int64) ([]*ScheduledTestPlan, error)
	ListDue(ctx context.Context, now time.Time) ([]*ScheduledTestPlan, error)
	Update(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error)
	Delete(ctx context.Context, id int64) error
	UpdateAfterRun(ctx context.Context, id int64, lastRunAt time.Time, nextRunAt time.Time) error
}

// ScheduledTestResultRepository defines the data access interface for test results.
type ScheduledTestResultRepository interface {
	Create(ctx context.Context, result *ScheduledTestResult) (*ScheduledTestResult, error)
	ListByPlanID(ctx context.Context, planID int64, limit int) ([]*ScheduledTestResult, error)
	PruneOldResults(ctx context.Context, planID int64, keepCount int) error
}
