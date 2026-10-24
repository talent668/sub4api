//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageTransportDetailsRoundTripAndHistoricalNull(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "route@test.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-route-test"})
	account := mustCreateAccount(t, client, &service.Account{Name: "route-test"})
	for i, details := range []*service.UsageTransportDetails{nil, {Client: "ws", Upstream: "http", Mode: "http_bridge", FallbackReason: "large_payload"}, {Client: "http", Upstream: "ws", Mode: "http_ws_bridge"}} {
		log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: []string{"history-route", "bridge-route", "native-route"}[i], Model: "gpt-5", OpenAIWSMode: true, TransportDetails: details, CreatedAt: time.Now()}
		inserted, err := repo.Create(ctx, log)
		require.NoError(t, err)
		require.True(t, inserted)
		got, err := repo.GetByID(ctx, log.ID)
		require.NoError(t, err)
		require.Equal(t, details, got.TransportDetails)
	}
}
