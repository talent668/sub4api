package repository

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/pkg/kimi"
	"github.com/stretchr/testify/require"
)

type kimiTestRoundTripper func(*http.Request) (*http.Response, error)

func (f kimiTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestKimiOAuthClientFixedHostDeviceIdentityAndRefreshForm(t *testing.T) {
	calls := 0
	client := &kimiOAuthClient{newClient: func(proxy string) (*http.Client, error) {
		require.Equal(t, "http://proxy.example:8080", proxy)
		return &http.Client{Transport: kimiTestRoundTripper(func(req *http.Request) (*http.Response, error) {
			calls++
			require.Equal(t, kimi.OAuthHost+"/api/oauth/token", req.URL.String())
			require.Equal(t, "device", req.Header.Get("X-Msh-Device-Id"))
			require.NoError(t, req.ParseForm())
			require.Equal(t, kimi.ClientID, req.Form.Get("client_id"))
			require.Equal(t, "refresh_token", req.Form.Get("grant_type"))
			require.Equal(t, "secret-refresh", req.Form.Get("refresh_token"))
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"access","expires_in":3600,"token_type":"Bearer"}`))}, nil
		})}, nil
	}}
	token, err := client.Refresh(context.Background(), "secret-refresh", "device", "http://proxy.example:8080")
	require.NoError(t, err)
	require.NoError(t, token.Validate())
	require.Equal(t, 1, calls)
}

func TestKimiOAuthClientNeverFollowsRedirectOrLeaksResponseSecrets(t *testing.T) {
	for _, status := range []int{302, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			client := &kimiOAuthClient{newClient: func(string) (*http.Client, error) {
				return &http.Client{Transport: kimiTestRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"https://attacker.example/steal"}}, Body: io.NopCloser(strings.NewReader(`{"detail":"secret-refresh"}`)), Request: req}, nil
				})}, nil
			}}
			_, err := client.Refresh(context.Background(), "secret-refresh", "device", "")
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-refresh")
			require.Equal(t, 1, calls)
		})
	}
}

func TestKimiOAuthClientProxyFailureDoesNotFallBack(t *testing.T) {
	client := &kimiOAuthClient{newClient: func(string) (*http.Client, error) { return nil, errors.New("proxy failed: secret-password") }}
	_, err := client.Refresh(context.Background(), "refresh", "device", "invalid")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret-password")
}
