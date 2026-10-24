package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/kimi"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type kimiTestSessions struct {
	mu     sync.Mutex
	values map[string][]byte
}

func (s *kimiTestSessions) Set(_ context.Context, id string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, e := json.Marshal(v)
	s.values[id] = b
	return e
}
func (s *kimiTestSessions) Get(_ context.Context, id string, v any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.values[id]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(b, v)
}
func (s *kimiTestSessions) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, id)
	return nil
}

type kimiTestLocks struct {
	GeminiTokenCache
	mu      sync.Mutex
	held    map[string]bool
	failure error
}

func (c *kimiTestLocks) AcquireRefreshLock(_ context.Context, key string, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return false, c.failure
	}
	if c.held[key] {
		return false, nil
	}
	c.held[key] = true
	return true, nil
}
func (c *kimiTestLocks) ReleaseRefreshLock(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, key)
	return nil
}

type kimiTestClient struct {
	startInterval int
	polls         int
	refreshes     int
	pollErr       error
	refreshErr    error
	onRefresh     func()
	token         *kimi.Token
	deviceID      string
}

func (c *kimiTestClient) Start(_ context.Context, deviceID, _ string) (*kimi.DeviceAuthorization, error) {
	c.deviceID = deviceID
	return &kimi.DeviceAuthorization{DeviceCode: "secret-device", UserCode: "ABCD-EFGH", VerificationURI: "https://auth.kimi.com/device", ExpiresIn: 600, Interval: c.startInterval}, nil
}
func (c *kimiTestClient) Poll(_ context.Context, _, deviceID, _ string) (*kimi.Token, error) {
	c.polls++
	if deviceID != c.deviceID {
		return nil, errors.New("device identity changed")
	}
	return c.token, c.pollErr
}
func (c *kimiTestClient) Refresh(context.Context, string, string, string) (*kimi.Token, error) {
	c.refreshes++
	if c.onRefresh != nil {
		c.onRefresh()
	}
	return c.token, c.refreshErr
}

func newKimiTestFlow() (*KimiOAuthService, *kimiTestClient, *kimiTestSessions, *kimiTestLocks) {
	client := &kimiTestClient{token: &kimi.Token{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600, TokenType: "Bearer"}}
	store := &kimiTestSessions{values: map[string][]byte{}}
	locks := &kimiTestLocks{held: map[string]bool{}}
	return NewKimiOAuthService(nil, nil, client, store, NewOAuthRefreshAPI(nil, locks)), client, store, locks
}

func allowKimiTestPoll(t *testing.T, store *kimiTestSessions, id string) {
	t.Helper()
	var session kimiDeviceSession
	ok, err := store.Get(context.Background(), id, &session)
	require.True(t, ok)
	require.NoError(t, err)
	session.NextPoll = time.Now().Add(-time.Second)
	require.NoError(t, store.Set(context.Background(), id, session))
}

func TestKimiDeviceAuthorizationOwnershipPacingAndSingleUse(t *testing.T) {
	s, client, store, _ := newKimiTestFlow()
	ctx := context.Background()
	grant, err := s.StartDevice(ctx, 11, nil)
	require.NoError(t, err)
	encoded, _ := json.Marshal(grant)
	require.NotContains(t, string(encoded), "secret-device")
	require.NotContains(t, string(encoded), "new-refresh")
	_, err = s.PollDevice(ctx, 12, grant.SessionID)
	require.Error(t, err)
	require.Zero(t, client.polls)
	result, err := s.PollDevice(ctx, 11, grant.SessionID)
	require.NoError(t, err)
	require.Equal(t, "pending", result.Status)
	require.Zero(t, client.polls)
	allowKimiTestPoll(t, store, grant.SessionID)
	result, err = s.PollDevice(ctx, 11, grant.SessionID)
	require.NoError(t, err)
	require.Equal(t, "authorized", result.Status)
	require.Equal(t, client.deviceID, result.Credentials["device_id"])
	require.Equal(t, AccountModeCoding, result.Credentials["account_mode"])
	_, err = s.PollDevice(ctx, 11, grant.SessionID)
	require.Error(t, err)
	require.Equal(t, 1, client.polls)
}

func TestKimiDeviceSlowDownAndExpiration(t *testing.T) {
	s, client, store, _ := newKimiTestFlow()
	ctx := context.Background()
	grant, err := s.StartDevice(ctx, 11, nil)
	require.NoError(t, err)
	allowKimiTestPoll(t, store, grant.SessionID)
	client.pollErr = errors.New("Kimi OAuth: slow_down")
	result, err := s.PollDevice(ctx, 11, grant.SessionID)
	require.NoError(t, err)
	require.Equal(t, 10, result.Interval)
	_, err = s.PollDevice(ctx, 11, grant.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, client.polls)
	var session kimiDeviceSession
	_, err = store.Get(ctx, grant.SessionID, &session)
	require.NoError(t, err)
	session.ExpiresAt = time.Now().Add(-time.Second)
	require.NoError(t, store.Set(ctx, grant.SessionID, session))
	_, err = s.PollDevice(ctx, 11, grant.SessionID)
	require.Error(t, err)
	require.Equal(t, 1, client.polls)
}

func TestKimiDeviceRedisFailureDoesNotPollUpstream(t *testing.T) {
	s, client, store, locks := newKimiTestFlow()
	grant, err := s.StartDevice(context.Background(), 11, nil)
	require.NoError(t, err)
	allowKimiTestPoll(t, store, grant.SessionID)
	locks.failure = errors.New("offline")
	_, err = s.PollDevice(context.Background(), 11, grant.SessionID)
	require.Error(t, err)
	require.Zero(t, client.polls)
}

func TestKimiDeviceRespectsLongIntervalsAndTransientOutages(t *testing.T) {
	s, client, store, _ := newKimiTestFlow()
	client.startInterval = 120
	grant, err := s.StartDevice(context.Background(), 11, nil)
	require.NoError(t, err)
	require.Equal(t, 120, grant.Interval)
	allowKimiTestPoll(t, store, grant.SessionID)
	client.pollErr = &kimi.OAuthError{StatusCode: 503}
	result, err := s.PollDevice(context.Background(), 11, grant.SessionID)
	require.NoError(t, err)
	require.Equal(t, "pending", result.Status)
	require.Equal(t, 125, result.Interval)
	_, err = s.PollDevice(context.Background(), 11, grant.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, client.polls)
}

type kimiTestAccountRepo struct {
	AccountRepository
	mu       sync.Mutex
	account  *Account
	casErr   error
	casCalls int
}

func (r *kimiTestAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshotOAuthRefreshAccount(r.account), nil
}
func (r *kimiTestAccountRepo) UpdateKimiOAuthCredentialsIfUnchanged(_ context.Context, _ int64, expected map[string]any, proxyID *int64, credentials map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.casCalls++
	if r.casErr != nil {
		return false, r.casErr
	}
	if !reflect.DeepEqual(expected, r.account.Credentials) || !reflect.DeepEqual(proxyID, r.account.ProxyID) {
		return false, nil
	}
	r.account.Credentials = shallowCopyMap(credentials)
	return true, nil
}

func (r *kimiTestAccountRepo) SetKimiOAuthRefreshErrorIfCredentialsUnchanged(_ context.Context, _ int64, expected map[string]any, proxyID *int64, _ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !reflect.DeepEqual(expected, r.account.Credentials) || !reflect.DeepEqual(proxyID, r.account.ProxyID) {
		return false, nil
	}
	r.account.Status = StatusError
	r.account.Schedulable = false
	return true, nil
}
func (r *kimiTestAccountRepo) SetKimiOAuthRefreshTempUnschedulableIfCredentialsUnchanged(_ context.Context, _ int64, expected map[string]any, proxyID *int64, until time.Time, reason string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !reflect.DeepEqual(expected, r.account.Credentials) || !reflect.DeepEqual(proxyID, r.account.ProxyID) {
		return false, nil
	}
	r.account.TempUnschedulableUntil = &until
	r.account.TempUnschedulableReason = reason
	return true, nil
}

func TestKimiBackgroundRefreshFailureCannotDisableReauthorizedAccount(t *testing.T) {
	repo := &kimiTestAccountRepo{account: kimiTestAccount()}
	attempt, _ := repo.GetByID(context.Background(), 1)
	repo.account.Credentials["refresh_token"] = "reauthorized"
	svc := &TokenRefreshService{accountRepo: repo}
	for _, permanent := range []bool{false, true} {
		err := svc.containKimiRefreshFailure(context.Background(), attempt, errors.New("invalid_grant"), permanent)
		require.ErrorIs(t, err, errRefreshSkipped)
		require.Equal(t, StatusActive, repo.account.Status)
		require.True(t, repo.account.Schedulable)
		require.Nil(t, repo.account.TempUnschedulableUntil)
	}
}

type kimiTestProxyRepo struct {
	ProxyRepository
	proxy *Proxy
}

func (r *kimiTestProxyRepo) GetByID(context.Context, int64) (*Proxy, error) { return r.proxy, nil }

func TestKimiMissingOrDisabledProxyNeverFallsBackToDirect(t *testing.T) {
	for _, proxy := range []*Proxy{nil, {ID: 7, Status: StatusDisabled}, {ID: 7, Status: StatusActive, ExpiresAt: func() *time.Time { v := time.Now().Add(-time.Minute); return &v }()}} {
		s, _, _, locks := newKimiTestFlow()
		repo := &kimiTestAccountRepo{account: kimiTestAccount()}
		repo.account.Credentials["expires_at"] = time.Now().Add(time.Hour).Unix()
		proxyID := int64(7)
		repo.account.ProxyID = &proxyID
		s.accounts = repo
		s.proxyRepo = &kimiTestProxyRepo{proxy: proxy}
		s.refreshAPI = NewOAuthRefreshAPI(repo, locks)
		fresh, err := s.GetAccount(context.Background(), repo.account, false)
		require.Error(t, err)
		require.Nil(t, fresh)
	}
}

type kimiTestSchedulerCache struct {
	SchedulerCache
	writes int
}

func (c *kimiTestSchedulerCache) SetAccount(context.Context, *Account) error { c.writes++; return nil }

func TestKimiRefreshDoesNotClearCooldownOrPublishOldSchedulerSnapshot(t *testing.T) {
	a := kimiTestAccount()
	until := time.Now().Add(time.Hour)
	a.TempUnschedulableUntil = &until
	a.TempUnschedulableReason = "security policy"
	cache := &kimiTestSchedulerCache{}
	svc := &TokenRefreshService{accountRepo: &kimiTestAccountRepo{account: a}, schedulerCache: cache}
	svc.postRefreshActions(context.Background(), a)
	svc.postRefreshStateSync(context.Background(), a)
	require.Equal(t, &until, a.TempUnschedulableUntil)
	require.Equal(t, "security policy", a.TempUnschedulableReason)
	require.Zero(t, cache.writes)
	err := svc.refreshWithRetry(context.Background(), a, &KimiTokenRefresher{}, nil, time.Minute)
	var configurationError *providerConfigurationRefreshError
	require.ErrorAs(t, err, &configurationError)
}

func kimiTestAccount() *Account {
	return &Account{ID: 1, Platform: PlatformKimi, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "old-access", "refresh_token": "old-refresh", "expires_at": time.Now().Add(-time.Minute).Unix(), "device_id": uuid.NewString(), "model_mapping": map[string]any{"alias": "kimi-for-coding"}}}
}

func TestKimiRefreshKeepsDeviceAndMappingAndReadsDurableCredentials(t *testing.T) {
	s, client, _, locks := newKimiTestFlow()
	repo := &kimiTestAccountRepo{account: kimiTestAccount()}
	s.accounts = repo
	s.refreshAPI = NewOAuthRefreshAPI(repo, locks)
	fresh, err := s.GetAccount(context.Background(), repo.account, false)
	require.NoError(t, err)
	require.Equal(t, "new-access", fresh.GetCredential("access_token"))
	require.Equal(t, "new-refresh", fresh.GetCredential("refresh_token"))
	require.NotEmpty(t, fresh.GetCredential("device_id"))
	require.Contains(t, fresh.Credentials, "model_mapping")
	repo.account.Credentials["access_token"] = "admin-new-access"
	fresh, err = s.GetAccount(context.Background(), repo.account, false)
	require.NoError(t, err)
	require.Equal(t, "admin-new-access", fresh.GetCredential("access_token"))
	require.Equal(t, 1, client.refreshes)
}

func TestKimiRefreshPreservesRefreshTokenWhenRotationOmitsIt(t *testing.T) {
	s, client, _, locks := newKimiTestFlow()
	repo := &kimiTestAccountRepo{account: kimiTestAccount()}
	s.accounts = repo
	s.refreshAPI = NewOAuthRefreshAPI(repo, locks)
	client.token.RefreshToken = ""
	fresh, err := s.GetAccount(context.Background(), repo.account, false)
	require.NoError(t, err)
	require.Equal(t, "old-refresh", fresh.GetCredential("refresh_token"))
}

func TestKimiConcurrentRequestsShareOneTokenRotation(t *testing.T) {
	s, client, _, locks := newKimiTestFlow()
	repo := &kimiTestAccountRepo{account: kimiTestAccount()}
	s.accounts = repo
	s.refreshAPI = NewOAuthRefreshAPI(repo, locks)
	entered := make(chan struct{})
	release := make(chan struct{})
	client.onRefresh = func() { close(entered); <-release }
	results := make(chan error, 2)
	account, _ := repo.GetByID(context.Background(), 1)
	go func() { _, err := s.GetAccount(context.Background(), account, false); results <- err }()
	<-entered
	go func() { _, err := s.GetAccount(context.Background(), account, false); results <- err }()
	close(release)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Equal(t, 1, client.refreshes)
	require.Equal(t, 1, repo.casCalls)
}

func TestKimiExpiredTokenWithRemoteRefreshLockDoesNotRequestReauthorization(t *testing.T) {
	s, client, _, locks := newKimiTestFlow()
	repo := &kimiTestAccountRepo{account: kimiTestAccount()}
	s.accounts = repo
	s.refreshAPI = NewOAuthRefreshAPI(repo, locks)
	locks.held["kimi:1"] = true
	_, err := s.GetAccount(context.Background(), repo.account, false)
	require.ErrorContains(t, err, "refresh is in progress")
	require.NotContains(t, err.Error(), "reauthorize")
	require.Zero(t, client.refreshes)
}

func TestKimiRefreshCannotOverwriteConcurrentReauthorization(t *testing.T) {
	s, client, _, locks := newKimiTestFlow()
	repo := &kimiTestAccountRepo{account: kimiTestAccount()}
	s.accounts = repo
	s.refreshAPI = NewOAuthRefreshAPI(repo, locks)
	client.onRefresh = func() {
		repo.account.Credentials = map[string]any{"access_token": "reauthorized", "refresh_token": "reauthorized-refresh", "device_id": uuid.NewString(), "expires_at": time.Now().Add(time.Hour).Unix()}
	}
	fresh, err := s.GetAccount(context.Background(), repo.account, false)
	require.NoError(t, err)
	require.Equal(t, "reauthorized", fresh.GetCredential("access_token"))
	require.Equal(t, 1, repo.casCalls)
}

func TestKimiRefreshPersistenceAndLockErrorsFailClosed(t *testing.T) {
	for _, failure := range []string{"cas", "lock"} {
		t.Run(failure, func(t *testing.T) {
			s, client, _, locks := newKimiTestFlow()
			repo := &kimiTestAccountRepo{account: kimiTestAccount()}
			s.accounts = repo
			s.refreshAPI = NewOAuthRefreshAPI(repo, locks)
			if failure == "cas" {
				repo.casErr = errors.New("write failed")
			} else {
				locks.failure = errors.New("offline")
			}
			fresh, err := s.GetAccount(context.Background(), repo.account, false)
			require.Error(t, err)
			require.Nil(t, fresh)
			require.Equal(t, "old-access", repo.account.GetCredential("access_token"))
			if failure == "lock" {
				require.Zero(t, client.refreshes)
			}
		})
	}
}

func TestKimiOAuthFixedEndpointsAndBearerHeaders(t *testing.T) {
	a := kimiTestAccount()
	a.Credentials["base_url"] = "https://attacker.example"
	a.Credentials["api_base_urls"] = map[string]any{APIProtocolAnthropic: "https://attacker.example"}
	require.Equal(t, DefaultKimiCodingBaseURL, a.GetOpenAIBaseURL())
	require.Equal(t, DefaultKimiCodingAnthropicBaseURL, a.GetCNProtocolBaseURL(APIProtocolAnthropic))
	require.Equal(t, AccountModeCoding, a.GetAccountMode())
	for _, target := range []string{"https://attacker.example/coding/v1/chat/completions", "https://api.kimi.com.attacker.example/coding/v1/chat/completions", "http://api.kimi.com/coding/v1/chat/completions", "https://api.kimi.com/other"} {
		req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader("{}"))
		require.Error(t, applyKimiOAuthRequestHeaders(req, a))
		require.Empty(t, req.Header.Get("Authorization"))
	}
	req, _ := http.NewRequest(http.MethodPost, DefaultKimiCodingBaseURL+"/chat/completions", nil)
	req.Header.Set("Cookie", "client-cookie")
	req.Header.Set("X-Api-Key", "client-key")
	setHeaderRaw(req.Header, "authorization", "Bearer downstream-secret")
	setHeaderRaw(req.Header, "x-msh-device-id", "spoofed-device")
	require.NoError(t, applyKimiOAuthRequestHeaders(req, a))
	require.Equal(t, "Bearer old-access", req.Header.Get("Authorization"))
	require.Empty(t, req.Header.Get("Cookie"))
	require.Empty(t, req.Header.Get("X-Api-Key"))
	require.NotContains(t, req.Header, "authorization")
	require.NotContains(t, req.Header, "x-msh-device-id")
	require.Equal(t, a.GetCredential("device_id"), req.Header.Get("X-Msh-Device-Id"))
	require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
}
