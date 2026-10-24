//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/pagination"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageLogSessionTPSAcrossPagesAndFilters(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "session-tps@test.com"})
	otherUser := mustCreateUser(t, client, &service.User{Email: "other-session-tps@test.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-session-tps"})
	otherKey := mustCreateApiKey(t, client, &service.APIKey{UserID: otherUser.ID, Key: "sk-other-session-tps"})
	account := mustCreateAccount(t, client, &service.Account{Name: "session-tps"})
	now := time.Now().UTC()
	insert := func(userID, keyID int64, model, sessionID string, output int, duration *int, kind service.RequestType, images, imageTokens int, mode string, at time.Time) {
		t.Helper()
		_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs
			(user_id, api_key_id, account_id, request_id, model, output_tokens, duration_ms,
			 request_type, image_count, image_output_tokens, billing_mode, session_id, created_at, image_size)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, CASE WHEN $9 > 0 THEN '1K' ELSE NULL END)`,
			userID, keyID, account.ID, fmt.Sprintf("session-tps-%d", time.Now().UnixNano()), model,
			output, duration, kind, images, imageTokens, mode, sessionID, at)
		require.NoError(t, err)
	}
	short, long, zero := 1000, 9000, 0
	// 100 / 1s and 100 / 9s: weighted session TPS is 20, not 55.6.
	insert(user.ID, key.ID, "current", "conversation", 100, &short, service.RequestTypeStream, 0, 0, "token", now)
	insert(user.ID, key.ID, "previous", "conversation", 100, &long, service.RequestTypeWSV2, 0, 0, "token", now.Add(-48*time.Hour))
	insert(otherUser.ID, otherKey.ID, "current", "conversation", 9000, &short, service.RequestTypeStream, 0, 0, "token", now)
	insert(user.ID, key.ID, "unrelated", "other-conversation", 9000, &short, service.RequestTypeStream, 0, 0, "token", now)
	insert(user.ID, key.ID, "invalid", "conversation", 9000, nil, service.RequestTypeStream, 0, 0, "token", now)
	insert(user.ID, key.ID, "invalid", "conversation", 9000, &zero, service.RequestTypeStream, 0, 0, "token", now)
	insert(user.ID, key.ID, "invalid", "conversation", 0, &short, service.RequestTypeStream, 0, 0, "token", now)
	insert(user.ID, key.ID, "invalid", "conversation", 9000, &short, service.RequestTypeLive, 0, 0, "token", now)
	insert(user.ID, key.ID, "invalid", "conversation", 9000, &short, service.RequestTypeUnknown, 0, 0, "token", now)
	insert(user.ID, key.ID, "invalid", "conversation", 9000, &short, service.RequestTypeSync, 1, 0, "token", now)
	insert(user.ID, key.ID, "invalid", "conversation", 9000, &short, service.RequestTypeSync, 0, 1, "token", now)
	insert(user.ID, key.ID, "invalid", "conversation", 9000, &short, service.RequestTypeSync, 0, 0, "image", now)
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	logs, _, err := repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 1}, UsageLogFilters{
		UserID: user.ID, Model: "current", StartTime: &start, EndTime: &end,
	})
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.InDelta(t, 20.0, *logs[0].SessionAverageOutputTPS, 1e-9)
	require.Equal(t, int64(2), logs[0].SessionTPSRequestCount)
	logs, _, err = repo.ListWithFilters(ctx, pagination.PaginationParams{Page: 1, PageSize: 1}, UsageLogFilters{UserID: user.ID, Model: "previous"})
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.InDelta(t, 20.0, *logs[0].SessionAverageOutputTPS, 1e-9)
}
