package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

const scheduledTestPlanColumns = `id, account_id, test_type, model_id, models, api_key_id, cron_expression, enabled, max_results, auto_recover, last_run_at, next_run_at, created_at, updated_at`

const scheduledTestResultColumns = `id, plan_id, model_id, status, response_text, error_message, predicted_model, probability, reason, http_status, latency_ms, started_at, finished_at, created_at`

// --- Plan Repository ---

type scheduledTestPlanRepository struct {
	db *sql.DB
}

func NewScheduledTestPlanRepository(db *sql.DB) service.ScheduledTestPlanRepository {
	return &scheduledTestPlanRepository{db: db}
}

func (r *scheduledTestPlanRepository) Create(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	models, err := marshalScheduledTestModels(plan.Models)
	if err != nil {
		return nil, err
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_plans (account_id, test_type, model_id, models, api_key_id, cron_expression, enabled, max_results, auto_recover, next_run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9, $10, NOW(), NOW())
		RETURNING `+scheduledTestPlanColumns,
		plan.AccountID, plan.TestType, plan.ModelID, models, plan.APIKeyID, plan.CronExpression, plan.Enabled, plan.MaxResults, plan.AutoRecover, plan.NextRunAt)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) GetByID(ctx context.Context, id int64) (*service.ScheduledTestPlan, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+scheduledTestPlanColumns+`
		FROM scheduled_test_plans WHERE id = $1
	`, id)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) ListByAccountID(ctx context.Context, accountID int64) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+scheduledTestPlanColumns+`
		FROM scheduled_test_plans WHERE account_id = $1
		ORDER BY created_at DESC
	`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) ListDue(ctx context.Context, now time.Time) ([]*service.ScheduledTestPlan, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+scheduledTestPlanColumns+`
		FROM scheduled_test_plans
		WHERE enabled = true AND next_run_at <= $1
		ORDER BY next_run_at ASC
	`, now)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanPlans(rows)
}

func (r *scheduledTestPlanRepository) Update(ctx context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	models, err := marshalScheduledTestModels(plan.Models)
	if err != nil {
		return nil, err
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE scheduled_test_plans
		SET test_type = $2, model_id = $3, models = $4::jsonb, api_key_id = $5, cron_expression = $6, enabled = $7, max_results = $8, auto_recover = $9, next_run_at = $10, updated_at = NOW()
		WHERE id = $1
		RETURNING `+scheduledTestPlanColumns,
		plan.ID, plan.TestType, plan.ModelID, models, plan.APIKeyID, plan.CronExpression, plan.Enabled, plan.MaxResults, plan.AutoRecover, plan.NextRunAt)
	return scanPlan(row)
}

func (r *scheduledTestPlanRepository) Delete(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM scheduled_test_plans WHERE id = $1`, id)
	return err
}

func (r *scheduledTestPlanRepository) UpdateAfterRun(ctx context.Context, id int64, lastRunAt time.Time, nextRunAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE scheduled_test_plans SET last_run_at = $2, next_run_at = $3, updated_at = NOW() WHERE id = $1
	`, id, lastRunAt, nextRunAt)
	return err
}

// --- Result Repository ---

type scheduledTestResultRepository struct {
	db *sql.DB
}

func NewScheduledTestResultRepository(db *sql.DB) service.ScheduledTestResultRepository {
	return &scheduledTestResultRepository{db: db}
}

func (r *scheduledTestResultRepository) Create(ctx context.Context, result *service.ScheduledTestResult) (*service.ScheduledTestResult, error) {
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO scheduled_test_results (plan_id, model_id, status, response_text, error_message, predicted_model, probability, reason, http_status, latency_ms, started_at, finished_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW())
		RETURNING `+scheduledTestResultColumns,
		result.PlanID, result.ModelID, result.Status, result.ResponseText, result.ErrorMessage, result.PredictedModel, result.Probability, result.Reason, result.HTTPStatus, result.LatencyMs, result.StartedAt, result.FinishedAt)
	return scanResult(row)
}

func (r *scheduledTestResultRepository) ListByPlanID(ctx context.Context, planID int64, limit int) ([]*service.ScheduledTestResult, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+scheduledTestResultColumns+`
		FROM scheduled_test_results
		WHERE plan_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, planID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []*service.ScheduledTestResult
	for rows.Next() {
		result, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func (r *scheduledTestResultRepository) PruneOldResults(ctx context.Context, planID int64, keepCount int) error {
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM scheduled_test_results
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY plan_id ORDER BY created_at DESC) AS rn
				FROM scheduled_test_results
				WHERE plan_id = $1
			) ranked
			WHERE rn > $2
		)
	`, planID, keepCount)
	return err
}

// --- scan helpers ---

type scannable interface {
	Scan(dest ...any) error
}

func marshalScheduledTestModels(models []string) (string, error) {
	if len(models) == 0 {
		return "[]", nil
	}
	encoded, err := json.Marshal(models)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func unmarshalScheduledTestModels(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var models []string
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, err
	}
	return models, nil
}

func scanPlan(row scannable) (*service.ScheduledTestPlan, error) {
	p := &service.ScheduledTestPlan{}
	var models []byte
	if err := row.Scan(
		&p.ID, &p.AccountID, &p.TestType, &p.ModelID, &models, &p.APIKeyID, &p.CronExpression,
		&p.Enabled, &p.MaxResults, &p.AutoRecover, &p.LastRunAt, &p.NextRunAt, &p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, err
	}
	decoded, err := unmarshalScheduledTestModels(models)
	if err != nil {
		return nil, err
	}
	p.Models = decoded
	return p, nil
}

func scanPlans(rows *sql.Rows) ([]*service.ScheduledTestPlan, error) {
	var plans []*service.ScheduledTestPlan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}

func scanResult(row scannable) (*service.ScheduledTestResult, error) {
	r := &service.ScheduledTestResult{}
	if err := row.Scan(
		&r.ID, &r.PlanID, &r.ModelID, &r.Status, &r.ResponseText, &r.ErrorMessage,
		&r.PredictedModel, &r.Probability, &r.Reason, &r.HTTPStatus,
		&r.LatencyMs, &r.StartedAt, &r.FinishedAt, &r.CreatedAt,
	); err != nil {
		return nil, err
	}
	return r, nil
}
