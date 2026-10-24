package repository

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	sharedhttp "github.com/MACOS-DO/sub4api/internal/pkg/httpclient"
	"github.com/MACOS-DO/sub4api/internal/pkg/kimi"
	"github.com/MACOS-DO/sub4api/internal/service"
)

type kimiOAuthClient struct {
	newClient func(string) (*http.Client, error)
}

func NewKimiOAuthClient() service.KimiOAuthClient { return &kimiOAuthClient{} }

func (c *kimiOAuthClient) post(ctx context.Context, path, deviceID, proxyURL string, form url.Values, dest any) error {
	newClient := c.newClient
	if newClient == nil {
		newClient = func(proxy string) (*http.Client, error) {
			return sharedhttp.GetClient(sharedhttp.Options{ProxyURL: proxy, Timeout: 30 * time.Second, ValidateResolvedIP: true})
		}
	}
	client, err := newClient(proxyURL)
	if err != nil {
		return errors.New("kimi OAuth proxy configuration is invalid")
	}
	// Copy the shared client before enforcing a no-redirect policy. Never send
	// device/refresh tokens to a redirect target, including another official host.
	local := *client
	local.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	form.Set("client_id", kimi.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kimi.OAuthHost+path, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("cannot build Kimi OAuth request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	kimi.SetDeviceHeaders(req.Header, deviceID)
	resp, err := local.Do(req)
	if err != nil {
		return &kimi.OAuthError{StatusCode: http.StatusServiceUnavailable}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		return errors.New("invalid Kimi OAuth response")
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return &kimi.OAuthError{StatusCode: resp.StatusCode}
	}
	var failure struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &failure)
	if failure.Error != "" {
		switch failure.Error {
		case "authorization_pending", "slow_down", "expired_token", "access_denied", "invalid_grant", "invalid_client", "invalid_scope":
			return &kimi.OAuthError{Code: failure.Error, StatusCode: resp.StatusCode}
		default:
			return errors.New("kimi OAuth authorization failed")
		}
	}
	if resp.StatusCode != http.StatusOK {
		return &kimi.OAuthError{StatusCode: resp.StatusCode}
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return errors.New("invalid Kimi OAuth response")
	}
	return nil
}

func (c *kimiOAuthClient) Start(ctx context.Context, deviceID, proxyURL string) (*kimi.DeviceAuthorization, error) {
	var result kimi.DeviceAuthorization
	err := c.post(ctx, "/api/oauth/device_authorization", deviceID, proxyURL, url.Values{}, &result)
	return &result, err
}

func (c *kimiOAuthClient) Poll(ctx context.Context, code, deviceID, proxyURL string) (*kimi.Token, error) {
	var result kimi.Token
	err := c.post(ctx, "/api/oauth/token", deviceID, proxyURL, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {code}}, &result)
	return &result, err
}

func (c *kimiOAuthClient) Refresh(ctx context.Context, refreshToken, deviceID, proxyURL string) (*kimi.Token, error) {
	var result kimi.Token
	err := c.post(ctx, "/api/oauth/token", deviceID, proxyURL, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}, &result)
	return &result, err
}
