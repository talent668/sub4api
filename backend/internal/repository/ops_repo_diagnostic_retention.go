package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
)

const (
	// Dedicated transaction-scoped lock, independent of the general ops cleaner.
	requestDiagnosticRetentionLockKey   int64 = 0x5355423444494147 // SUB4DIAG
	requestDiagnosticRetentionTimeout         = 10 * time.Second
	requestDiagnosticRetentionWorkTime        = 8 * time.Second
	requestDiagnosticRetentionBatchSize       = 500
	requestDiagnosticRetentionDeleteCap       = 5000
	requestDiagnosticRetentionScanCap         = 250000
	requestDiagnosticRetentionPageCap         = 500

	requestDiagnosticRetentionTimeoutSQL = `SELECT set_config('statement_timeout', '2000ms', true)`
	requestDiagnosticRetentionLockSQL    = `SELECT pg_try_advisory_xact_lock($1)`

	requestDiagnosticExpiredDeleteSQL = `
WITH batch AS (
  SELECT id FROM ops_system_logs
  WHERE ` + service.OpsRequestDiagnosticLogSQLPredicate + ` AND created_at < $1
  ORDER BY created_at ASC, id ASC
  LIMIT $2
)
DELETE FROM ops_system_logs
WHERE ` + service.OpsRequestDiagnosticLogSQLPredicate + ` AND id IN (SELECT id FROM batch)`

	// pg_column_size(l) estimates the logical stored whole-row size, NOT table,
	// index, free-space or physical disk usage. Never fetch JSON/payloads here.
	requestDiagnosticSizeFirstPageSQL = `
SELECT id, created_at, pg_column_size(l)::bigint
FROM ops_system_logs AS l
WHERE ` + service.OpsRequestDiagnosticLogSQLPredicate + `
ORDER BY created_at DESC, id DESC
LIMIT $1`
	requestDiagnosticSizeNextPageSQL = `
SELECT id, created_at, pg_column_size(l)::bigint
FROM ops_system_logs AS l
WHERE ` + service.OpsRequestDiagnosticLogSQLPredicate + ` AND (created_at, id) < ($1, $2)
ORDER BY created_at DESC, id DESC
LIMIT $3`

	// Include the crossing row itself: even a single newest oversized row must
	// eventually be removed. Delete from the oldest end, not from the scan end.
	requestDiagnosticSizeDeleteSQL = `
WITH batch AS (
  SELECT id FROM ops_system_logs
  WHERE ` + service.OpsRequestDiagnosticLogSQLPredicate + ` AND (created_at, id) <= ($1, $2)
  ORDER BY created_at ASC, id ASC
  LIMIT $3
)
DELETE FROM ops_system_logs
WHERE ` + service.OpsRequestDiagnosticLogSQLPredicate + ` AND id IN (SELECT id FROM batch)`
)

var _ service.RequestDiagnosticLogMaintainer = (*opsRepository)(nil)

// MaintainRequestDiagnosticLogs is an optional repository capability. One pass
// is bounded in duration, scanned rows/pages and deleted rows across BOTH age
// and size. No COUNT/SUM, table-size polling, TRUNCATE or schema changes are used.
func (r *opsRepository) MaintainRequestDiagnosticLogs(ctx context.Context, policy service.RequestDiagnosticRetentionPolicy) (service.RequestDiagnosticRetentionResult, error) {
	return r.maintainRequestDiagnosticLogs(ctx, policy, time.Now().UTC())
}

func (r *opsRepository) maintainRequestDiagnosticLogs(ctx context.Context, policy service.RequestDiagnosticRetentionPolicy, now time.Time) (service.RequestDiagnosticRetentionResult, error) {
	var out service.RequestDiagnosticRetentionResult
	if r == nil || r.db == nil {
		return out, fmt.Errorf("nil ops repository")
	}
	if policy.RetentionDays < 1 || policy.RetentionDays > 3650 || policy.MaxSizeMB < 0 || policy.MaxSizeMB > 102400 {
		return out, fmt.Errorf("invalid request diagnostic retention policy")
	}
	ctx, cancel := context.WithTimeout(ctx, requestDiagnosticRetentionTimeout)
	defer cancel()
	stopAt := time.Now().Add(requestDiagnosticRetentionWorkTime)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, requestDiagnosticRetentionTimeoutSQL); err != nil {
		return out, err
	}
	var locked bool
	if err := tx.QueryRowContext(ctx, requestDiagnosticRetentionLockSQL, requestDiagnosticRetentionLockKey).Scan(&locked); err != nil {
		return out, err
	}
	if !locked {
		out.LockSkipped = true
		return out, nil
	}

	// Keep working counts private until commit; errors must not report rolled
	// back deletes as successful maintenance.
	var working service.RequestDiagnosticRetentionResult
	cutoff := now.AddDate(0, 0, -policy.RetentionDays)
	for time.Now().Before(stopAt) {
		limit := min(requestDiagnosticRetentionBatchSize, requestDiagnosticRetentionDeleteCap-int(working.DeletedExpired))
		deleted, err := requestDiagnosticDelete(ctx, tx, requestDiagnosticExpiredDeleteSQL, cutoff, limit)
		if err != nil {
			return out, err
		}
		working.DeletedExpired += deleted
		if working.DeletedExpired >= requestDiagnosticRetentionDeleteCap {
			working.HasMore = true
			break
		}
		if deleted < int64(limit) {
			break
		}
	}
	if !time.Now().Before(stopAt) {
		working.HasMore = true
	}

	if policy.MaxSizeMB > 0 && !working.HasMore {
		// MiB is explicitly a logical whole-row estimate; settings are bounded
		// before multiplication, avoiding integer overflow even on 32-bit hosts.
		budget := int64(policy.MaxSizeMB) * 1024 * 1024
		boundary, err := requestDiagnosticSizeCutoff(ctx, tx, budget, stopAt, &working)
		if err != nil {
			return out, err
		}
		if boundary != nil {
			for time.Now().Before(stopAt) {
				remaining := requestDiagnosticRetentionDeleteCap - int(working.DeletedExpired+working.DeletedForSize)
				limit := min(requestDiagnosticRetentionBatchSize, remaining)
				deleted, err := requestDiagnosticDelete(ctx, tx, requestDiagnosticSizeDeleteSQL, boundary.createdAt, boundary.id, limit)
				if err != nil {
					return out, err
				}
				working.DeletedForSize += deleted
				if deleted >= int64(remaining) {
					working.HasMore = true
					break
				}
				if deleted < int64(limit) {
					break
				}
			}
			if !time.Now().Before(stopAt) {
				working.HasMore = true
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return working, nil
}

type requestDiagnosticRowKey struct {
	id        int64
	createdAt time.Time
}

func requestDiagnosticSizeCutoff(ctx context.Context, tx *sql.Tx, budget int64, stopAt time.Time, result *service.RequestDiagnosticRetentionResult) (*requestDiagnosticRowKey, error) {
	return requestDiagnosticSizeCutoffWithLimits(ctx, tx, budget, stopAt, result, requestDiagnosticRetentionScanCap, requestDiagnosticRetentionPageCap)
}

func requestDiagnosticSizeCutoffWithLimits(ctx context.Context, tx *sql.Tx, budget int64, stopAt time.Time, result *service.RequestDiagnosticRetentionResult, scanCap, pageCap int) (*requestDiagnosticRowKey, error) {
	var cursor *requestDiagnosticRowKey
	var retainedBytes int64
	for page := 0; page < pageCap && result.ScannedRows < int64(scanCap) && time.Now().Before(stopAt); page++ {
		limit := min(requestDiagnosticRetentionBatchSize, scanCap-int(result.ScannedRows))
		q, args := requestDiagnosticSizeFirstPageSQL, []any{limit}
		if cursor != nil {
			q, args = requestDiagnosticSizeNextPageSQL, []any{cursor.createdAt, cursor.id, limit}
		}
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		var pageRows int
		var crossing *requestDiagnosticRowKey
		for rows.Next() {
			var key requestDiagnosticRowKey
			var rowBytes int64
			if err := rows.Scan(&key.id, &key.createdAt, &rowBytes); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if rowBytes <= 0 {
				_ = rows.Close()
				return nil, fmt.Errorf("invalid request diagnostic row size")
			}
			result.ScannedRows++
			pageRows++
			cursor = &key
			if rowBytes > budget-retainedBytes {
				crossing = &key
				break
			}
			retainedBytes += rowBytes
		}
		rowErr := rows.Err()
		closeErr := rows.Close()
		if rowErr != nil {
			return nil, rowErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if crossing != nil {
			return crossing, nil
		}
		if pageRows < limit {
			return nil, nil // complete scan and every row fits, including equality
		}
	}
	// We have NOT proved that the budget is exceeded. Never guess a cutoff or
	// delete on a partial scan whose newest retained rows still fit. Each pass
	// restarts at newest: neither a stale cumulative byte count nor a cursor is
	// safe across other instances/manual deletions. ScanIncomplete therefore
	// means the quota is NOT guaranteed, not that another pass will converge.
	result.ScanIncomplete = true
	result.HasMore = true
	return nil, nil
}

func requestDiagnosticDelete(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
