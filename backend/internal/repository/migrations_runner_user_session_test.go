package repository

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MACOS-DO/sub4api/migrations"
	"github.com/stretchr/testify/require"
)

func TestUserSessionIndexMigrationRepairsInvalidIndexBeforeRetry(t *testing.T) {
	for _, invalid := range []bool{true, false} {
		name := "preserve-valid-index"
		if invalid {
			name = "rebuild-invalid-index"
		}
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			content, err := migrations.FS.ReadFile(usageLogsUserSessionIndexMigration)
			require.NoError(t, err)
			prepareMigrationsBootstrapExpectations(mock)
			mock.ExpectQuery("SELECT checksum FROM schema_migrations WHERE filename = \\$1").
				WithArgs(usageLogsUserSessionIndexMigration).WillReturnError(sql.ErrNoRows)
			mock.ExpectQuery("SELECT EXISTS \\(").WithArgs(usageLogsUserSessionIndex).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(invalid))
			if invalid {
				mock.ExpectExec("DROP INDEX CONCURRENTLY IF EXISTS " + usageLogsUserSessionIndex).
					WillReturnResult(sqlmock.NewResult(0, 0))
			}
			mock.ExpectExec("CREATE INDEX CONCURRENTLY IF NOT EXISTS " + usageLogsUserSessionIndex).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec("INSERT INTO schema_migrations \\(filename, checksum\\) VALUES \\(\\$1, \\$2\\)").
				WithArgs(usageLogsUserSessionIndexMigration, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec("SELECT pg_advisory_unlock\\(\\$1\\)").WithArgs(migrationsAdvisoryLockID).
				WillReturnResult(sqlmock.NewResult(0, 1))
			err = applyMigrationsFS(context.Background(), db, fstest.MapFS{
				usageLogsUserSessionIndexMigration: &fstest.MapFile{Data: content},
			})
			require.NoError(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
