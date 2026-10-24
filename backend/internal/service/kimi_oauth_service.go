package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/MACOS-DO/sub4api/internal/pkg/errors"
	"github.com/MACOS-DO/sub4api/internal/pkg/kimi"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type KimiOAuthClient interface {
	Start(context.Context, string, string) (*kimi.DeviceAuthorization, error)
	Poll(context.Context, string, string, string) (*kimi.Token, error)
	Refresh(context.Context, string, string, string) (*kimi.Token, error)
}

type KimiOAuthSessionStore interface {
	Set(context.Context, string, any) error
	Get(context.Context, string, any) (bool, error)
	Delete(context.Context, string) error
}

type kimiDeviceSession struct {
	OwnerID    int64     `json:"owner_id"`
	DeviceCode string    `json:"device_code"`
	DeviceID   string    `json:"device_id"`
	ProxyURL   string    `json:"proxy_url"`
	ExpiresAt  time.Time `json:"expires_at"`
	NextPoll   time.Time `json:"next_poll"`
	Interval   int       `json:"interval"`
	Completed  bool      `json:"completed"`
}

type KimiOAuthService struct {
	proxyRepo  ProxyRepository
	accounts   AccountRepository
	client     KimiOAuthClient
	sessions   KimiOAuthSessionStore
	refreshAPI *OAuthRefreshAPI
}

func NewKimiOAuthService(proxies ProxyRepository, accounts AccountRepository, client KimiOAuthClient, sessions KimiOAuthSessionStore, refreshAPI *OAuthRefreshAPI) *KimiOAuthService {
	return &KimiOAuthService{proxyRepo: proxies, accounts: accounts, client: client, sessions: sessions, refreshAPI: refreshAPI}
}

type KimiDeviceResult struct {
	SessionID       string `json:"session_id"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type KimiPollResult struct {
	Status      string         `json:"status"`
	Interval    int            `json:"interval"`
	Credentials map[string]any `json:"credentials,omitempty"`
}

func (s *KimiOAuthService) proxyURL(ctx context.Context, id *int64) (string, error) {
	p, err := s.loadProxy(ctx, id)
	if err != nil || p == nil {
		return "", err
	}
	return p.URL(), nil
}

func (s *KimiOAuthService) loadProxy(ctx context.Context, id *int64) (*Proxy, error) {
	if id == nil {
		return nil, nil
	}
	if s.proxyRepo == nil {
		return nil, errors.New("proxy repository is unavailable")
	}
	p, err := s.proxyRepo.GetByID(ctx, *id)
	if err != nil || p == nil || p.ID != *id || !p.IsActive() || p.IsExpired(time.Now()) {
		return nil, infraerrors.BadRequest("KIMI_PROXY_NOT_FOUND", "Selected proxy is unavailable")
	}
	return p, nil
}

func (s *KimiOAuthService) StartDevice(ctx context.Context, ownerID int64, proxyID *int64) (*KimiDeviceResult, error) {
	if ownerID <= 0 || s.sessions == nil || s.client == nil {
		return nil, errors.New("kimi OAuth is unavailable")
	}
	if s.refreshAPI == nil || s.refreshAPI.tokenCache == nil {
		return nil, errors.New("kimi authorization storage is unavailable")
	}
	allowed, err := s.refreshAPI.tokenCache.AcquireRefreshLock(ctx, "kimi-device-start:"+strconv.FormatInt(ownerID, 10), 5*time.Second)
	if err != nil {
		return nil, errors.New("kimi authorization storage is unavailable")
	}
	if !allowed {
		return nil, infraerrors.New(http.StatusTooManyRequests, "KIMI_AUTH_RATE_LIMITED", "Please wait before requesting another Kimi authorization code")
	}
	proxyURL, err := s.proxyURL(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	deviceID := uuid.NewString()
	grant, err := s.client.Start(ctx, deviceID, proxyURL)
	if err != nil {
		return nil, err
	}
	if grant == nil || grant.DeviceCode == "" || grant.UserCode == "" || grant.ExpiresIn <= 0 || !kimi.ValidateVerificationURL(grant.VerificationURI) {
		return nil, errors.New("invalid Kimi device authorization response")
	}
	verificationURL := grant.VerificationURI
	if grant.VerificationURIComplete != "" {
		if !kimi.ValidateVerificationURL(grant.VerificationURIComplete) {
			return nil, errors.New("invalid Kimi verification URL")
		}
		verificationURL = grant.VerificationURIComplete
	}
	interval := max(5, min(grant.Interval, 15*60))
	expiresIn := min(grant.ExpiresIn, 15*60)
	now := time.Now()
	id := uuid.NewString()
	session := kimiDeviceSession{OwnerID: ownerID, DeviceCode: grant.DeviceCode, DeviceID: deviceID, ProxyURL: proxyURL, ExpiresAt: now.Add(time.Duration(expiresIn) * time.Second), NextPoll: now.Add(time.Duration(interval) * time.Second), Interval: interval}
	if err := s.sessions.Set(ctx, id, session); err != nil {
		return nil, errors.New("cannot store Kimi authorization session")
	}
	return &KimiDeviceResult{SessionID: id, UserCode: grant.UserCode, VerificationURI: verificationURL, ExpiresIn: expiresIn, Interval: interval}, nil
}

func (s *KimiOAuthService) PollDevice(ctx context.Context, ownerID int64, id string) (*KimiPollResult, error) {
	if _, err := uuid.Parse(id); err != nil || ownerID <= 0 {
		return nil, infraerrors.BadRequest("KIMI_SESSION_INVALID", "Authorization session is invalid or expired")
	}
	if s.sessions == nil || s.refreshAPI == nil || s.refreshAPI.tokenCache == nil {
		return nil, errors.New("kimi authorization storage is unavailable")
	}
	// The shared lock serializes polls across app replicas. A storage outage
	// fails closed instead of creating duplicate upstream token exchanges.
	key := "kimi-device:" + id
	locked, err := s.refreshAPI.tokenCache.AcquireRefreshLock(ctx, key, time.Minute)
	if err != nil {
		return nil, errors.New("kimi authorization storage is unavailable")
	}
	if !locked {
		return &KimiPollResult{Status: "pending", Interval: 5}, nil
	}
	defer s.refreshAPI.releaseRefreshLock(ctx, key)
	var session kimiDeviceSession
	ok, err := s.sessions.Get(ctx, id, &session)
	if err != nil {
		return nil, errors.New("kimi authorization storage is unavailable")
	}
	if !ok || session.OwnerID != ownerID || session.Completed || !time.Now().Before(session.ExpiresAt) {
		return nil, infraerrors.BadRequest("KIMI_SESSION_INVALID", "Authorization session is invalid or expired")
	}
	if time.Now().Before(session.NextPoll) {
		return &KimiPollResult{Status: "pending", Interval: session.Interval}, nil
	}
	session.NextPoll = time.Now().Add(time.Duration(session.Interval) * time.Second)
	if err := s.sessions.Set(ctx, id, session); err != nil {
		return nil, errors.New("kimi authorization storage is unavailable")
	}
	token, err := s.client.Poll(ctx, session.DeviceCode, session.DeviceID, session.ProxyURL)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "authorization_pending"):
			return &KimiPollResult{Status: "pending", Interval: session.Interval}, nil
		case strings.Contains(err.Error(), "slow_down"):
			session.Interval = min(session.Interval+5, 15*60)
			session.NextPoll = time.Now().Add(time.Duration(session.Interval) * time.Second)
			if err := s.sessions.Set(ctx, id, session); err != nil {
				return nil, errors.New("kimi authorization storage is unavailable")
			}
			return &KimiPollResult{Status: "pending", Interval: session.Interval}, nil
		default:
			var upstreamError *kimi.OAuthError
			if errors.As(err, &upstreamError) && upstreamError.Temporary() {
				session.Interval = min(session.Interval+5, 15*60)
				session.NextPoll = time.Now().Add(time.Duration(session.Interval) * time.Second)
				if err := s.sessions.Set(ctx, id, session); err != nil {
					return nil, errors.New("kimi authorization storage is unavailable")
				}
				return &KimiPollResult{Status: "pending", Interval: session.Interval}, nil
			}
			if strings.Contains(err.Error(), "expired_token") || strings.Contains(err.Error(), "access_denied") {
				_ = s.sessions.Delete(ctx, id)
			}
			return nil, err
		}
	}
	if err := token.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(token.RefreshToken) == "" {
		return nil, errors.New("kimi authorization did not return a refresh token")
	}
	session.Completed = true
	session.DeviceCode = ""
	if err := s.sessions.Set(ctx, id, session); err != nil {
		return nil, errors.New("cannot complete Kimi authorization session")
	}
	return &KimiPollResult{Status: "authorized", Credentials: kimiTokenCredentials(token, session.DeviceID)}, nil
}

func kimiTokenCredentials(token *kimi.Token, deviceID string) map[string]any {
	credentials := map[string]any{"access_token": token.AccessToken, "token_type": "Bearer", "expires_at": time.Now().Add(time.Duration(token.ExpiresIn * float64(time.Second))).Unix(), "scope": token.Scope, "device_id": deviceID, "account_mode": AccountModeCoding, "api_protocol": APIProtocolAdaptive}
	if token.RefreshToken != "" {
		credentials["refresh_token"] = token.RefreshToken
	}
	return credentials
}

// GetAccount rereads durable credentials for every request; no token hot cache.
func (s *KimiOAuthService) GetAccount(ctx context.Context, account *Account, force bool) (*Account, error) {
	if s == nil || s.accounts == nil || s.refreshAPI == nil || account == nil {
		return nil, errors.New("kimi OAuth is unavailable")
	}
	fresh, err := s.accounts.GetByID(ctx, account.ID)
	if err != nil {
		return nil, errors.New("cannot read Kimi OAuth account")
	}
	if fresh == nil || fresh.ID != account.ID || !fresh.IsKimiOAuth() || !fresh.IsActive() {
		return nil, errors.New("kimi OAuth account is unavailable")
	}
	executor := &KimiTokenRefresher{service: s, force: force}
	refreshInProgress := false
	if executor.NeedsRefresh(fresh, 5*time.Minute) {
		result, refreshErr := s.refreshAPI.RefreshIfNeeded(withOAuthRefreshRequestPath(ctx), fresh, executor, 5*time.Minute)
		if refreshErr != nil {
			return nil, refreshErr
		}
		if result != nil && result.Account != nil {
			fresh = result.Account
		}
		if result != nil && result.LockHeld {
			refreshInProgress = true
			fresh, err = s.accounts.GetByID(ctx, account.ID)
			if err != nil || fresh == nil {
				return nil, errors.New("cannot read Kimi OAuth credentials")
			}
		}
	}
	if fresh.ID != account.ID || !fresh.IsKimiOAuth() || !fresh.IsActive() {
		return nil, errors.New("kimi OAuth account changed during refresh")
	}
	expiresAt := fresh.GetCredentialAsTime("expires_at")
	if fresh.GetCredential("access_token") == "" || expiresAt == nil || !time.Now().Before(*expiresAt) {
		if refreshInProgress {
			return nil, errors.New("kimi OAuth refresh is in progress; retry shortly")
		}
		return nil, errors.New("kimi OAuth token expired; reauthorize this account")
	}
	if fresh.ProxyID != nil {
		proxy, err := s.loadProxy(ctx, fresh.ProxyID)
		if err != nil {
			return nil, err
		}
		fresh.Proxy = proxy
	}
	return fresh, nil
}

type KimiTokenRefresher struct {
	service *KimiOAuthService
	force   bool
}

func (r *KimiTokenRefresher) CacheKey(a *Account) string {
	return "kimi:" + strconv.FormatInt(a.ID, 10)
}
func (r *KimiTokenRefresher) CanRefresh(a *Account) bool {
	return a != nil && a.IsKimiOAuth() && a.GetCredential("refresh_token") != ""
}
func (r *KimiTokenRefresher) NeedsRefresh(a *Account, window time.Duration) bool {
	if a == nil || !a.IsKimiOAuth() {
		return false
	}
	expires := a.GetCredentialAsTime("expires_at")
	return r.force || a.GetCredential("access_token") == "" || expires == nil || time.Until(*expires) < window
}
func (r *KimiTokenRefresher) Refresh(ctx context.Context, a *Account) (map[string]any, error) {
	if r.service == nil || r.service.client == nil {
		return nil, errors.New("kimi OAuth is unavailable")
	}
	proxyURL, err := r.service.proxyURL(ctx, a.ProxyID)
	if err != nil {
		return nil, err
	}
	deviceID := a.GetCredential("device_id")
	if _, err := uuid.Parse(deviceID); err != nil {
		return nil, errors.New("kimi OAuth device identity is missing; reauthorize this account")
	}
	token, err := r.service.client.Refresh(ctx, a.GetCredential("refresh_token"), deviceID, proxyURL)
	if err != nil {
		return nil, err
	}
	if err := token.Validate(); err != nil {
		return nil, err
	}
	return MergeCredentials(a.Credentials, kimiTokenCredentials(token, deviceID)), nil
}

type KimiOAuthRefreshRepository interface {
	UpdateKimiOAuthCredentialsIfUnchanged(context.Context, int64, map[string]any, *int64, map[string]any) (bool, error)
}

func (a *Account) IsKimiOAuth() bool {
	return a != nil && a.Platform == PlatformKimi && a.Type == AccountTypeOAuth
}

type KimiOAuthRefreshMutationRepository interface {
	SetKimiOAuthRefreshErrorIfCredentialsUnchanged(context.Context, int64, map[string]any, *int64, string) (bool, error)
	SetKimiOAuthRefreshTempUnschedulableIfCredentialsUnchanged(context.Context, int64, map[string]any, *int64, time.Time, string) (bool, error)
}

func (s *TokenRefreshService) SetKimiOAuthService(oauth *KimiOAuthService) {
	refresher := &KimiTokenRefresher{service: oauth}
	s.registrations = append(s.registrations, tokenRefreshRegistration{platform: PlatformKimi, refresher: refresher, executor: refresher})
}

func (s *TokenRefreshService) containKimiRefreshFailure(ctx context.Context, a *Account, cause error, permanent bool) error {
	repo, ok := s.accountRepo.(KimiOAuthRefreshMutationRepository)
	if !ok {
		return &providerConfigurationRefreshError{err: errors.New("kimi conditional refresh storage is unavailable")}
	}
	var applied bool
	var err error
	until := time.Now().Add(5 * time.Minute)
	if permanent {
		applied, err = repo.SetKimiOAuthRefreshErrorIfCredentialsUnchanged(ctx, a.ID, a.Credentials, a.ProxyID, "Kimi OAuth authorization expired; reauthorize this account")
	} else {
		applied, err = repo.SetKimiOAuthRefreshTempUnschedulableIfCredentialsUnchanged(ctx, a.ID, a.Credentials, a.ProxyID, until, "token refresh retry exhausted: Kimi OAuth refresh temporarily unavailable")
	}
	if err != nil {
		return &providerCycleContainmentRefreshError{err: errors.New("cannot persist Kimi OAuth refresh failure")}
	}
	if !applied {
		return errRefreshSkipped
	}
	if permanent {
		until = time.Time{}
	}
	s.notifyAccountSchedulingBlocked(a, until, "kimi_oauth_refresh_failed")
	if cause == nil {
		cause = errors.New("kimi OAuth refresh failed")
	}
	if permanent {
		return &accountPermanentRefreshError{err: cause, persistentlyBlocked: true}
	}
	return &providerCycleContainmentRefreshError{err: cause}
}

func normalizeKimiOAuthCredentials(credentials map[string]any) (map[string]any, error) {
	a := &Account{Platform: PlatformKimi, Type: AccountTypeOAuth, Credentials: credentials}
	if a.GetCredential("access_token") == "" || a.GetCredential("refresh_token") == "" {
		return nil, infraerrors.BadRequest("KIMI_TOKEN_REQUIRED", "Kimi OAuth requires access and refresh tokens")
	}
	if _, err := uuid.Parse(a.GetCredential("device_id")); err != nil {
		return nil, infraerrors.BadRequest("KIMI_DEVICE_REQUIRED", "Kimi OAuth requires its authorized device identity")
	}
	out := shallowCopyMap(credentials)
	for _, key := range []string{"api_key", "base_url", "api_base_urls", "client_id", "oauth_host", "token_url"} {
		delete(out, key)
	}
	out["account_mode"] = AccountModeCoding
	out["api_protocol"] = APIProtocolAdaptive
	return out, nil
}

func kimiUnavailableError(err error) error {
	if strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "reauthorize") {
		return &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeAccount, Reason: "kimi_oauth_credentials_unavailable", NextAccountAction: NextAccountRetry, ClientStatusCode: http.StatusBadGateway, ClientMessage: "Kimi OAuth authorization is unavailable; reauthorize this account"}
	}
	return infraerrors.New(http.StatusBadGateway, "KIMI_OAUTH_UNAVAILABLE", fmt.Sprintf("Kimi OAuth failed: %s", err.Error()))
}

type kimiPreparedAccountContextKey struct{}

func (s *OpenAIGatewayService) prepareKimiOAuthAccount(ctx context.Context, c *gin.Context, a *Account) (*Account, error) {
	fresh, err := s.kimiOAuthService.GetAccount(ctx, a, false)
	if err != nil {
		return nil, kimiUnavailableError(err)
	}
	if !fresh.IsSchedulable() {
		return nil, errors.New("kimi OAuth account is not schedulable")
	}
	if c != nil {
		var groupID *int64
		if apiKey := getAPIKeyFromContext(c); apiKey != nil {
			groupID = apiKey.GroupID
		}
		if !s.openAIAccountMatchesSchedulingGroup(fresh, groupID) {
			return nil, errors.New("kimi account group changed before forwarding")
		}
	}
	return fresh, nil
}

func applyKimiOAuthRequestHeaders(req *http.Request, a *Account) error {
	if !a.IsKimiOAuth() {
		return nil
	}
	if req.URL == nil || req.URL.Scheme != "https" || req.URL.Host != "api.kimi.com" || req.URL.User != nil || !strings.HasPrefix(req.URL.Path, "/coding/") {
		return errors.New("kimi OAuth credentials may only be sent to the official Coding API")
	}
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization", "Chatgpt-Account-Id", "OpenAI-Organization", "OpenAI-Project", "X-Codex-Turn-State"} {
		deleteHeaderAllForms(req.Header, name)
	}
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-msh-") {
			delete(req.Header, name)
		}
	}
	req.Header.Set("Authorization", "Bearer "+a.GetCredential("access_token"))
	kimi.SetDeviceHeaders(req.Header, a.GetCredential("device_id"))
	*req = *req.WithContext(WithHTTPUpstreamRedirectsDisabled(req.Context()))
	return nil
}
