package service

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestOpsRequestDiagnosticLogSQLPredicateIsExactAndNullSafe(t *testing.T) {
	require.Equal(t, `COALESCE(component = 'gateway.request_diagnostics' AND message = 'openai.messages_ws_latency' AND extra->>'event' = 'openai.messages_ws_latency', FALSE)`, OpsRequestDiagnosticLogSQLPredicate)
	// COALESCE wraps the entire conjunction, so NOT(predicate) also selects
	// ordinary logs with missing/null extra.event, rather than losing them to
	// SQL's three-valued NOT(NULL) behavior.
	require.True(t, strings.HasPrefix(OpsRequestDiagnosticLogSQLPredicate, "COALESCE("))
	require.True(t, strings.HasSuffix(OpsRequestDiagnosticLogSQLPredicate, ", FALSE)"))
}

func TestOpsCleanupSystemLogsAlwaysExcludeDiagnostics(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "age", true: "days_zero"}[all], func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			cutoff := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
			where := "created_at < $1 AND "
			limit := "$2"
			if all {
				where, limit = "", "$1"
			}
			q := `WITH batch AS ( SELECT id FROM ops_system_logs WHERE ` + where + `NOT (` + OpsRequestDiagnosticLogSQLPredicate + `) ORDER BY id LIMIT ` + limit + ` ) DELETE FROM ops_system_logs WHERE id IN (SELECT id FROM batch) AND NOT (` + OpsRequestDiagnosticLogSQLPredicate + `)`
			for _, n := range []int64{3, 0} {
				expect := mock.ExpectExec(q)
				if all {
					expect.WithArgs(opsCleanupBatchSize)
				} else {
					expect.WithArgs(cutoff, 2)
				}
				expect.WillReturnResult(sqlmock.NewResult(0, n))
			}
			n, err := opsCleanupRunOne(context.Background(), db, all, cutoff, "ops_system_logs", "created_at", false, 2)
			require.NoError(t, err)
			require.Equal(t, int64(3), n)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpsCleanupDaysZeroOtherTablesStillTruncateIncludingAudit(t *testing.T) {
	for _, table := range []string{"ops_error_logs", "ops_ingress_reject_aggregates", "ops_alert_events", "ops_system_log_cleanup_audits", "ops_system_metrics", "ops_metrics_hourly", "ops_metrics_daily"} {
		t.Run(table, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM " + table)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
			mock.ExpectExec("^TRUNCATE TABLE " + table + "$").WillReturnResult(sqlmock.NewResult(0, 0))
			n, err := opsCleanupRunOne(context.Background(), db, true, time.Time{}, table, "created_at", false, 2)
			require.NoError(t, err)
			require.Equal(t, int64(7), n)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpsCleanupOtherTableAgeSQLUnchanged(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cutoff := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	q := `WITH batch AS ( SELECT id FROM ops_metrics_daily WHERE bucket_date < $1::date ORDER BY id LIMIT $2 ) DELETE FROM ops_metrics_daily WHERE id IN (SELECT id FROM batch)`
	mock.ExpectExec(q).WithArgs(cutoff, 2).WillReturnResult(sqlmock.NewResult(0, 0))
	_, err = opsCleanupRunOne(context.Background(), db, false, cutoff, "ops_metrics_daily", "bucket_date", true, 2)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
