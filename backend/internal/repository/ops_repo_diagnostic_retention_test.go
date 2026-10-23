package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func diagnosticRetentionTestRepo(t *testing.T) (*opsRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &opsRepository{db: db}, mock
}

func diagnosticRetentionExpectStart(mock sqlmock.Sqlmock, locked bool) {
	mock.ExpectBegin()
	mock.ExpectExec(requestDiagnosticRetentionTimeoutSQL).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(requestDiagnosticRetentionLockSQL).WithArgs(requestDiagnosticRetentionLockKey).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(locked))
}

func diagnosticRetentionExpectAge(mock sqlmock.Sqlmock, now time.Time, days int, deleted int64) {
	mock.ExpectExec(requestDiagnosticExpiredDeleteSQL).WithArgs(now.AddDate(0, 0, -days), requestDiagnosticRetentionBatchSize).
		WillReturnResult(sqlmock.NewResult(0, deleted))
}

func diagnosticRetentionSizeRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "created_at", "row_bytes"})
}

func TestRequestDiagnosticRetentionSQLScopeOrderingAndBounds(t *testing.T) {
	for name, q := range map[string]string{
		"age": requestDiagnosticExpiredDeleteSQL, "first_page": requestDiagnosticSizeFirstPageSQL,
		"next_page": requestDiagnosticSizeNextPageSQL, "size_delete": requestDiagnosticSizeDeleteSQL,
	} {
		t.Run(name, func(t *testing.T) {
			require.Contains(t, q, service.OpsRequestDiagnosticLogSQLPredicate)
			require.Contains(t, q, "LIMIT $")
			for _, forbidden := range []string{"COUNT(", "SUM(", "pg_total_relation_size", "pg_relation_size", "TRUNCATE", "OFFSET", "SELECT *"} {
				require.NotContains(t, q, forbidden)
			}
		})
	}
	require.Contains(t, requestDiagnosticExpiredDeleteSQL, "created_at < $1")
	require.Contains(t, requestDiagnosticExpiredDeleteSQL, "ORDER BY created_at ASC, id ASC")
	require.Contains(t, requestDiagnosticSizeDeleteSQL, "(created_at, id) <= ($1, $2)")
	require.Contains(t, requestDiagnosticSizeDeleteSQL, "ORDER BY created_at ASC, id ASC")
	require.Contains(t, requestDiagnosticSizeNextPageSQL, "(created_at, id) < ($1, $2)")
	for _, q := range []string{requestDiagnosticSizeFirstPageSQL, requestDiagnosticSizeNextPageSQL} {
		require.Contains(t, q, "SELECT id, created_at, pg_column_size(l)::bigint")
		require.Contains(t, q, "ORDER BY created_at DESC, id DESC")
		projection := strings.SplitN(q, "FROM", 2)[0]
		require.NotContains(t, projection, "extra")
	}
	require.Contains(t, requestDiagnosticRetentionTimeoutSQL, "set_config('statement_timeout', '2000ms', true)")
	require.LessOrEqual(t, requestDiagnosticRetentionTimeout, 10*time.Second)
	require.LessOrEqual(t, requestDiagnosticRetentionDeleteCap, 10000)
	require.LessOrEqual(t, requestDiagnosticRetentionScanCap, 250000)
}

func TestMaintainRequestDiagnosticLogsSizeDisabledStillExpires(t *testing.T) {
	repo, mock := diagnosticRetentionTestRepo(t)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	diagnosticRetentionExpectStart(mock, true)
	diagnosticRetentionExpectAge(mock, now, 7, 3)
	mock.ExpectCommit()
	out, err := repo.maintainRequestDiagnosticLogs(context.Background(), service.RequestDiagnosticRetentionPolicy{RetentionDays: 7}, now)
	require.NoError(t, err)
	require.Equal(t, service.RequestDiagnosticRetentionResult{DeletedExpired: 3}, out)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMaintainRequestDiagnosticLogsUnavailableLockSkipsAndReleasesTransaction(t *testing.T) {
	repo, mock := diagnosticRetentionTestRepo(t)
	diagnosticRetentionExpectStart(mock, false)
	mock.ExpectRollback()
	out, err := repo.MaintainRequestDiagnosticLogs(context.Background(), service.RequestDiagnosticRetentionPolicy{RetentionDays: 7, MaxSizeMB: 1})
	require.NoError(t, err)
	require.Equal(t, service.RequestDiagnosticRetentionResult{LockSkipped: true}, out)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMaintainRequestDiagnosticLogsAgeDeleteCap(t *testing.T) {
	repo, mock := diagnosticRetentionTestRepo(t)
	now := time.Now().UTC()
	diagnosticRetentionExpectStart(mock, true)
	for n := 0; n < requestDiagnosticRetentionDeleteCap/requestDiagnosticRetentionBatchSize; n++ {
		diagnosticRetentionExpectAge(mock, now, 7, requestDiagnosticRetentionBatchSize)
	}
	mock.ExpectCommit()
	out, err := repo.maintainRequestDiagnosticLogs(context.Background(), service.RequestDiagnosticRetentionPolicy{RetentionDays: 7, MaxSizeMB: 1}, now)
	require.NoError(t, err)
	require.Equal(t, service.RequestDiagnosticRetentionResult{DeletedExpired: requestDiagnosticRetentionDeleteCap, HasMore: true}, out)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMaintainRequestDiagnosticLogsSizeEqualityIsRetained(t *testing.T) {
	repo, mock := diagnosticRetentionTestRepo(t)
	now := time.Now().UTC()
	diagnosticRetentionExpectStart(mock, true)
	diagnosticRetentionExpectAge(mock, now, 7, 0)
	mock.ExpectQuery(requestDiagnosticSizeFirstPageSQL).WithArgs(requestDiagnosticRetentionBatchSize).
		WillReturnRows(diagnosticRetentionSizeRows().AddRow(7, now, 1024*1024))
	mock.ExpectCommit()
	out, err := repo.maintainRequestDiagnosticLogs(context.Background(), service.RequestDiagnosticRetentionPolicy{RetentionDays: 7, MaxSizeMB: 1}, now)
	require.NoError(t, err)
	require.Equal(t, service.RequestDiagnosticRetentionResult{ScannedRows: 1}, out)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMaintainRequestDiagnosticLogsCrossingRowCutoffIsInclusive(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "equality_then_crossing_tied_timestamp", true: "newest_oversized"}[oversized], func(t *testing.T) {
			repo, mock := diagnosticRetentionTestRepo(t)
			now := time.Now().UTC()
			diagnosticRetentionExpectStart(mock, true)
			diagnosticRetentionExpectAge(mock, now, 7, 0)
			rows := diagnosticRetentionSizeRows()
			boundaryID, scanned := int64(6), int64(2)
			if oversized {
				rows.AddRow(7, now, 1024*1024+1)
				boundaryID, scanned = 7, 1
			} else {
				rows.AddRow(7, now, 1024*1024).AddRow(6, now, 1)
			}
			mock.ExpectQuery(requestDiagnosticSizeFirstPageSQL).WithArgs(requestDiagnosticRetentionBatchSize).WillReturnRows(rows)
			mock.ExpectExec(requestDiagnosticSizeDeleteSQL).WithArgs(now, boundaryID, requestDiagnosticRetentionBatchSize).
				WillReturnResult(sqlmock.NewResult(0, 3))
			mock.ExpectCommit()
			out, err := repo.maintainRequestDiagnosticLogs(context.Background(), service.RequestDiagnosticRetentionPolicy{RetentionDays: 7, MaxSizeMB: 1}, now)
			require.NoError(t, err)
			require.Equal(t, service.RequestDiagnosticRetentionResult{DeletedForSize: 3, ScannedRows: scanned}, out)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMaintainRequestDiagnosticLogsSizeCrossingOnNextKeysetPage(t *testing.T) {
	repo, mock := diagnosticRetentionTestRepo(t)
	now := time.Now().UTC()
	diagnosticRetentionExpectStart(mock, true)
	diagnosticRetentionExpectAge(mock, now, 7, 0)
	rows := diagnosticRetentionSizeRows()
	for id := 1000; id > 500; id-- {
		rows.AddRow(id, now, 2048)
	}
	mock.ExpectQuery(requestDiagnosticSizeFirstPageSQL).WithArgs(requestDiagnosticRetentionBatchSize).WillReturnRows(rows)
	// The page cursor is exclusive, and id breaks ties in created_at.
	mock.ExpectQuery(requestDiagnosticSizeNextPageSQL).WithArgs(now, int64(501), requestDiagnosticRetentionBatchSize).
		WillReturnRows(diagnosticRetentionSizeRows().AddRow(500, now, 24576).AddRow(499, now, 1))
	mock.ExpectExec(requestDiagnosticSizeDeleteSQL).WithArgs(now, int64(499), requestDiagnosticRetentionBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	out, err := repo.maintainRequestDiagnosticLogs(context.Background(), service.RequestDiagnosticRetentionPolicy{RetentionDays: 7, MaxSizeMB: 1}, now)
	require.NoError(t, err)
	require.Equal(t, service.RequestDiagnosticRetentionResult{DeletedForSize: 1, ScannedRows: 502}, out)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMaintainRequestDiagnosticLogsAgeAndSizeShareTotalDeleteCap(t *testing.T) {
	repo, mock := diagnosticRetentionTestRepo(t)
	now := time.Now().UTC()
	diagnosticRetentionExpectStart(mock, true)
	for n := 0; n < 9; n++ {
		diagnosticRetentionExpectAge(mock, now, 7, 500)
	}
	diagnosticRetentionExpectAge(mock, now, 7, 499)
	mock.ExpectQuery(requestDiagnosticSizeFirstPageSQL).WithArgs(requestDiagnosticRetentionBatchSize).
		WillReturnRows(diagnosticRetentionSizeRows().AddRow(7, now, 1024*1024+1))
	mock.ExpectExec(requestDiagnosticSizeDeleteSQL).WithArgs(now, int64(7), 1).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	out, err := repo.maintainRequestDiagnosticLogs(context.Background(), service.RequestDiagnosticRetentionPolicy{RetentionDays: 7, MaxSizeMB: 1}, now)
	require.NoError(t, err)
	require.Equal(t, service.RequestDiagnosticRetentionResult{DeletedExpired: 4999, DeletedForSize: 1, ScannedRows: 1, HasMore: true}, out)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRequestDiagnosticIncompleteScanNeverChoosesDeletionCutoff(t *testing.T) {
	// Exercise the identical bounded scan with a small private limit instead of
	// materializing 250000 mock rows; verify row and page bounds independently.
	for _, pageLimited := range []bool{false, true} {
		t.Run(map[bool]string{false: "row_limit", true: "page_limit"}[pageLimited], func(t *testing.T) {
			repo, mock := diagnosticRetentionTestRepo(t)
			now := time.Now().UTC()
			mock.ExpectBegin()
			tx, err := repo.db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			const scannedRows = 1000
			for page := 0; page < 2; page++ {
				rows := diagnosticRetentionSizeRows()
				for n := 0; n < requestDiagnosticRetentionBatchSize; n++ {
					rows.AddRow(scannedRows-page*requestDiagnosticRetentionBatchSize-n, now, 1)
				}
				if page == 0 {
					mock.ExpectQuery(requestDiagnosticSizeFirstPageSQL).WithArgs(requestDiagnosticRetentionBatchSize).WillReturnRows(rows)
				} else {
					mock.ExpectQuery(requestDiagnosticSizeNextPageSQL).WithArgs(now, int64(501), requestDiagnosticRetentionBatchSize).WillReturnRows(rows)
				}
			}
			scanCap, pageCap := scannedRows, 40
			if pageLimited {
				scanCap, pageCap = requestDiagnosticRetentionScanCap, 2
			}
			var out service.RequestDiagnosticRetentionResult
			boundary, err := requestDiagnosticSizeCutoffWithLimits(context.Background(), tx, 1024*1024, time.Now().Add(time.Second), &out, scanCap, pageCap)
			require.NoError(t, err)
			require.Nil(t, boundary)
			require.Equal(t, service.RequestDiagnosticRetentionResult{ScannedRows: scannedRows, ScanIncomplete: true, HasMore: true}, out)
			mock.ExpectRollback()
			require.NoError(t, tx.Rollback())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestRequestDiagnosticSizeCutoffElapsedWorkBudgetDoesNotDelete(t *testing.T) {
	repo, mock := diagnosticRetentionTestRepo(t)
	mock.ExpectBegin()
	tx, err := repo.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	var out service.RequestDiagnosticRetentionResult
	boundary, err := requestDiagnosticSizeCutoff(context.Background(), tx, 1024*1024, time.Now().Add(-time.Second), &out)
	require.NoError(t, err)
	require.Nil(t, boundary)
	require.True(t, out.ScanIncomplete)
	require.True(t, out.HasMore)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMaintainRequestDiagnosticLogsErrorRollsBackAllCounts(t *testing.T) {
	boom := errors.New("retention failure")
	for _, phase := range []string{"begin", "timeout", "lock", "age", "affected", "scan_query", "scan_value", "scan_rows", "size_delete", "commit"} {
		t.Run(phase, func(t *testing.T) {
			repo, mock := diagnosticRetentionTestRepo(t)
			now := time.Now().UTC()
			if phase == "begin" {
				mock.ExpectBegin().WillReturnError(boom)
			} else {
				mock.ExpectBegin()
				if phase == "timeout" {
					mock.ExpectExec(requestDiagnosticRetentionTimeoutSQL).WillReturnError(boom)
				} else {
					mock.ExpectExec(requestDiagnosticRetentionTimeoutSQL).WillReturnResult(sqlmock.NewResult(0, 1))
					if phase == "lock" {
						mock.ExpectQuery(requestDiagnosticRetentionLockSQL).WithArgs(requestDiagnosticRetentionLockKey).WillReturnError(boom)
					} else {
						mock.ExpectQuery(requestDiagnosticRetentionLockSQL).WithArgs(requestDiagnosticRetentionLockKey).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
						switch phase {
						case "age":
							mock.ExpectExec(requestDiagnosticExpiredDeleteSQL).WillReturnError(boom)
						case "affected":
							mock.ExpectExec(requestDiagnosticExpiredDeleteSQL).WillReturnResult(sqlmock.NewErrorResult(boom))
						default:
							diagnosticRetentionExpectAge(mock, now, 7, 2)
							scan := mock.ExpectQuery(requestDiagnosticSizeFirstPageSQL).WithArgs(requestDiagnosticRetentionBatchSize)
							switch phase {
							case "scan_query":
								scan.WillReturnError(boom)
							case "scan_value":
								scan.WillReturnRows(diagnosticRetentionSizeRows().AddRow(7, now, "not an integer"))
							case "scan_rows":
								scan.WillReturnRows(diagnosticRetentionSizeRows().AddRow(7, now, 1).RowError(0, boom))
							case "size_delete":
								scan.WillReturnRows(diagnosticRetentionSizeRows().AddRow(7, now, 1024*1024+1))
								mock.ExpectExec(requestDiagnosticSizeDeleteSQL).WillReturnError(boom)
							case "commit":
								scan.WillReturnRows(diagnosticRetentionSizeRows())
								mock.ExpectCommit().WillReturnError(boom)
							}
						}
					}
				}
				if phase != "commit" {
					mock.ExpectRollback()
				}
			}
			out, err := repo.maintainRequestDiagnosticLogs(context.Background(), service.RequestDiagnosticRetentionPolicy{RetentionDays: 7, MaxSizeMB: 1}, now)
			require.Error(t, err)
			require.Equal(t, service.RequestDiagnosticRetentionResult{}, out)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestMaintainRequestDiagnosticLogsRejectsInvalidPolicyWithoutDBWork(t *testing.T) {
	repo, mock := diagnosticRetentionTestRepo(t)
	for _, policy := range []service.RequestDiagnosticRetentionPolicy{
		{RetentionDays: -1}, {RetentionDays: 0}, {RetentionDays: 3651},
		{RetentionDays: 7, MaxSizeMB: -1}, {RetentionDays: 7, MaxSizeMB: 102401},
	} {
		out, err := repo.MaintainRequestDiagnosticLogs(context.Background(), policy)
		require.Error(t, err)
		require.Equal(t, service.RequestDiagnosticRetentionResult{}, out)
	}
	var nilRepo *opsRepository
	_, err := nilRepo.MaintainRequestDiagnosticLogs(context.Background(), service.RequestDiagnosticRetentionPolicy{RetentionDays: 7})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Notify when the holder has reached its first delete, so the second invocation
// attempts the advisory lock while the holder's transaction is still open.
type diagnosticRetentionStartedArg struct{ started chan struct{} }

func (a diagnosticRetentionStartedArg) Match(driver.Value) bool {
	select {
	case <-a.started:
	default:
		close(a.started)
	}
	return true
}

func TestMaintainRequestDiagnosticLogsConcurrentInvocationSkipsLockedPass(t *testing.T) {
	repo, mock := diagnosticRetentionTestRepo(t)
	mock.MatchExpectationsInOrder(false)
	now := time.Now().UTC()
	diagnosticRetentionExpectStart(mock, true)
	started := make(chan struct{})
	mock.ExpectExec(requestDiagnosticExpiredDeleteSQL).WithArgs(diagnosticRetentionStartedArg{started}, requestDiagnosticRetentionBatchSize).
		WillDelayFor(100 * time.Millisecond).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	diagnosticRetentionExpectStart(mock, false)
	mock.ExpectRollback()
	type outcome struct {
		result service.RequestDiagnosticRetentionResult
		err    error
	}
	done := make(chan outcome, 1)
	policy := service.RequestDiagnosticRetentionPolicy{RetentionDays: 7}
	go func() {
		result, err := repo.maintainRequestDiagnosticLogs(context.Background(), policy, now)
		done <- outcome{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first maintenance did not reach its delete")
	}
	second, err := repo.maintainRequestDiagnosticLogs(context.Background(), policy, now)
	require.NoError(t, err)
	require.True(t, second.LockSkipped)
	first := <-done
	require.NoError(t, first.err)
	require.Equal(t, int64(1), first.result.DeletedExpired)
	require.NoError(t, mock.ExpectationsWereMet())
}
