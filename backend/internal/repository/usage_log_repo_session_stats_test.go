package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageLogSessionStatsBatchAndUserIsolation(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	sessionID := "same-session"
	otherSession := "other-session"
	logs := []service.UsageLog{
		{UserID: 1, SessionID: &sessionID},
		{UserID: 1, SessionID: &sessionID},
		{UserID: 2, SessionID: &sessionID},
		{UserID: 1, SessionID: &otherSession},
		{UserID: 1},
	}
	// No page, date, model, API key or account filter may truncate a conversation.
	mock.ExpectQuery(`(?s)SUM\(ul.output_tokens\)::double precision \* 1000 / SUM\(ul.duration_ms\)::double precision.*JOIN \(VALUES.*ON ul.user_id = sessions.user_id AND ul.session_id = sessions.session_id.*ul.output_tokens > 0 AND ul.duration_ms > 0.*ul.image_count = 0 AND ul.image_output_tokens = 0.*COALESCE\(ul.billing_mode, ''\) <> 'image'.*ul.request_type IN \(1, 2, 3, 4\).*GROUP BY ul.user_id, ul.session_id`).
		WithArgs(int64(1), sessionID, int64(2), sessionID, int64(1), otherSession).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "session_id", "rate", "requests"}).
			AddRow(1, sessionID, 62.5, 4).AddRow(2, sessionID, 10.0, 2))
	require.NoError(t, repo.hydrateUsageLogSessionStats(context.Background(), logs))
	for _, i := range []int{0, 1} {
		require.Equal(t, 62.5, *logs[i].SessionAverageOutputTPS)
		require.Equal(t, int64(4), logs[i].SessionTPSRequestCount)
	}
	require.Equal(t, 10.0, *logs[2].SessionAverageOutputTPS)
	require.Equal(t, int64(2), logs[2].SessionTPSRequestCount)
	for _, i := range []int{3, 4} {
		require.Nil(t, logs[i].SessionAverageOutputTPS)
		require.Zero(t, logs[i].SessionTPSRequestCount)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageLogSessionStatsSkipsMissingIdentity(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	empty, sessionID := " ", "session"
	logs := []service.UsageLog{{UserID: 1}, {UserID: 1, SessionID: &empty}, {SessionID: &sessionID}}
	require.NoError(t, repo.hydrateUsageLogSessionStats(context.Background(), logs))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageLogSessionStatsPropagatesQueryError(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	sessionID := "session"
	mock.ExpectQuery("SELECT ul.user_id, ul.session_id").WithArgs(int64(1), sessionID).WillReturnError(fmt.Errorf("query failed"))
	require.EqualError(t, repo.hydrateUsageLogSessionStats(context.Background(), []service.UsageLog{{UserID: 1, SessionID: &sessionID}}), "query failed")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageLogSessionStatsBestEffortPreservesRowsOnFailure(t *testing.T) {
	for _, failure := range []string{"query", "timeout", "scan"} {
		t.Run(failure, func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}
			sessionID := "session"
			logs := []service.UsageLog{{UserID: 1, SessionID: &sessionID, RequestID: "keep-this-row", OutputTokens: 100}}
			expectation := mock.ExpectQuery("SELECT ul.user_id, ul.session_id").WithArgs(int64(1), sessionID)
			switch failure {
			case "query":
				expectation.WillReturnError(fmt.Errorf("statistics unavailable"))
			case "timeout":
				expectation.WillDelayFor(usageSessionStatsTimeout + 250*time.Millisecond).
					WillReturnRows(sqlmock.NewRows([]string{"user_id", "session_id", "rate", "requests"}).AddRow(1, sessionID, 50.0, 2))
			case "scan":
				expectation.WillReturnRows(sqlmock.NewRows([]string{"user_id", "session_id", "rate", "requests"}).AddRow(1, sessionID, "invalid rate", 2))
			}
			require.NoError(t, repo.hydrateUsageLogSessionStatsBestEffort(context.Background(), logs))
			require.Len(t, logs, 1)
			require.Equal(t, "keep-this-row", logs[0].RequestID)
			require.Equal(t, 100, logs[0].OutputTokens)
			require.Nil(t, logs[0].SessionAverageOutputTPS)
			require.Zero(t, logs[0].SessionTPSRequestCount)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUsageLogSessionStatsBestEffortHonorsCallerCancellation(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, repo.hydrateUsageLogSessionStatsBestEffort(ctx, nil), context.Canceled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageLogSessionStatsBestEffortHonorsCallerDeadlineDuringQuery(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	sessionID := "session"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	mock.ExpectQuery("SELECT ul.user_id, ul.session_id").WithArgs(int64(1), sessionID).
		WillDelayFor(usageSessionStatsTimeout).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "session_id", "rate", "requests"}).AddRow(1, sessionID, 50.0, 2))
	require.ErrorIs(t, repo.hydrateUsageLogSessionStatsBestEffort(ctx, []service.UsageLog{{UserID: 1, SessionID: &sessionID}}), context.DeadlineExceeded)
	require.NoError(t, mock.ExpectationsWereMet())
}
