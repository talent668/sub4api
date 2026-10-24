package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/stretchr/testify/require"
)

type authoritativeTicketLifecycle struct {
	CodexTicketLifecycleRepository
	mu      sync.Mutex
	tickets map[string]json.RawMessage
	calls   int
	err     error
	started chan struct{}
	release chan struct{}
	block   bool
}

func (repo *authoritativeTicketLifecycle) CurrentTicket(ctx context.Context, accountID int64, model string) (json.RawMessage, error) {
	repo.mu.Lock()
	raw := append(json.RawMessage(nil), repo.tickets[openAICodexTicketKey(accountID, model)]...)
	repo.calls++
	err, block := repo.err, repo.block
	repo.block = false
	repo.mu.Unlock()
	if block {
		close(repo.started)
		select {
		case <-repo.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return raw, err
}

func (repo *authoritativeTicketLifecycle) Invalidate(_ context.Context, event *CodexTicketInvalidation) (bool, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	key := openAICodexTicketKey(event.AccountID, event.Model)
	var current openAICodexTicket
	if err := json.Unmarshal(repo.tickets[key], &current); err != nil {
		return false, err
	}
	event.InvalidatedCurrent = current.GenerationID == event.TicketGenerationID
	if event.InvalidatedCurrent {
		delete(repo.tickets, key)
	}
	return true, nil
}

func (repo *authoritativeTicketLifecycle) set(t *testing.T, ticket *openAICodexTicket) {
	t.Helper()
	raw, err := json.Marshal(ticket)
	require.NoError(t, err)
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.tickets == nil {
		repo.tickets = make(map[string]json.RawMessage)
	}
	repo.tickets[openAICodexTicketKey(ticket.AccountID, ticket.Model)] = raw
}

func TestCodexTicketAuthorityReadsEveryRequestAndIgnoresCaptureClock(t *testing.T) {
	account := ticketTestAccount(41)
	old := verifiedTicket(account, "gpt-6-astra", "old-state", "__oailb=old")
	old.CapturedAt = time.Now().Add(time.Hour)
	current := verifiedTicket(account, old.Model, "current-state", "__oailb=current")
	current.CapturedAt = old.CapturedAt.Add(-2 * time.Hour)
	repo := &authoritativeTicketLifecycle{}
	repo.set(t, old)
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, nil)
	svc.openaiCodexTicketLifecycle = repo
	svc.openaiCodexTickets.Store(openAICodexTicketKey(account.ID, old.Model), old)
	first := http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, old.Model, first))
	require.Equal(t, old.State, first.Get(openAICodexTurnStateHeader))
	repo.set(t, current)
	second := http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, old.Model, second))
	require.Equal(t, current.State, second.Get(openAICodexTurnStateHeader))
	require.Equal(t, current.Cookie, second.Get("Cookie"))
	require.Equal(t, 2, repo.calls)
	// Neither another account nor another model may reuse this cached ticket.
	for _, target := range []struct {
		account *Account
		model   string
	}{{ticketTestAccount(42), old.Model}, {account, "gpt-5.6-sol"}} {
		headers := http.Header{}
		require.ErrorIs(t, svc.applyOpenAICodexTicket(context.Background(), target.account, target.model, headers), ErrOpenAICodexTicketUnavailable)
		require.Empty(t, headers.Get(openAICodexTurnStateHeader))
		require.Empty(t, headers.Get("Cookie"))
	}
}

func TestCodexTicketAuthorityLateReadCannotRepublishInvalidatedTicket(t *testing.T) {
	account := ticketTestAccount(41)
	old := verifiedTicket(account, "gpt-6-astra", "old-state", "__oailb=old")
	repo := &authoritativeTicketLifecycle{started: make(chan struct{}), release: make(chan struct{}), block: true}
	repo.set(t, old)
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, nil)
	svc.openaiCodexTicketLifecycle = repo
	key := openAICodexTicketKey(account.ID, old.Model)
	svc.openaiCodexTickets.Store(key, old)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- svc.applyOpenAICodexTicket(ctx, account, old.Model, http.Header{}) }()
	select {
	case <-repo.started:
	case <-ctx.Done():
		t.Fatal("ticket lookup did not start")
	}
	// Invalidate after the old DB snapshot was read, before it can return.
	svc.observeCodexTicketResponse(ticketObservationRequest(t, old), changedTicketResponse(), account)
	close(repo.release)
	require.NoError(t, <-result)
	_, cached := svc.openaiCodexTickets.Load(key)
	require.False(t, cached, "a late request-side read must not restore an invalidated cache entry")
	for range 3 {
		headers := http.Header{}
		require.ErrorIs(t, svc.applyOpenAICodexTicket(context.Background(), account, old.Model, headers), ErrOpenAICodexTicketUnavailable)
		require.Empty(t, headers.Get(openAICodexTurnStateHeader))
		require.Empty(t, headers.Get("Cookie"))
	}
}

func TestCodexTicketAuthorityLookupFailureDoesNotReuseCache(t *testing.T) {
	account := ticketTestAccount(41)
	ticket := verifiedTicket(account, "gpt-6-astra", "cached-state", "__oailb=cached")
	wantErr := errors.New("lookup unavailable")
	repo := &authoritativeTicketLifecycle{err: wantErr}
	svc := ticketTestService(t, config.OpenAICodexTicketConfig{Enabled: true, FailClosed: true}, nil)
	svc.openaiCodexTicketLifecycle = repo
	svc.openaiCodexTickets.Store(openAICodexTicketKey(account.ID, ticket.Model), ticket)
	headers := http.Header{}
	require.ErrorIs(t, svc.applyOpenAICodexTicket(context.Background(), account, ticket.Model, headers), wantErr)
	require.Empty(t, headers.Get(openAICodexTurnStateHeader))
	require.Empty(t, headers.Get("Cookie"))
}
