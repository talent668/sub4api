// Package kimi defines the official Kimi Code device authorization protocol.
// Protocol reference: MoonshotAI/kimi-cli, klips/klip-14-kimi-code-oauth-login.md.
package kimi

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
)

const (
	ClientID   = "17e5f671-d194-4dfb-9706-5516cb48c098"
	OAuthHost  = "https://auth.kimi.com"
	APIBaseURL = "https://api.kimi.com/coding/v1"
)

type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type Token struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	TokenType    string  `json:"token_type"`
	Scope        string  `json:"scope"`
	ExpiresIn    float64 `json:"expires_in"`
	Error        string  `json:"error"`
}

// OAuthError carries only allowlisted protocol codes, never an upstream body.
type OAuthError struct {
	Code       string
	StatusCode int
}

func (e *OAuthError) Error() string {
	if e.Code != "" {
		return "Kimi OAuth: " + e.Code
	}
	return fmt.Sprintf("Kimi OAuth service returned HTTP %d", e.StatusCode)
}
func (e *OAuthError) Temporary() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

func (t *Token) Validate() error {
	if t == nil || strings.TrimSpace(t.AccessToken) == "" || math.IsNaN(t.ExpiresIn) || math.IsInf(t.ExpiresIn, 0) || t.ExpiresIn <= 0 || t.ExpiresIn > 366*24*3600 {
		return errors.New("invalid Kimi OAuth token response")
	}
	if t.TokenType != "" && !strings.EqualFold(t.TokenType, "Bearer") {
		return errors.New("unsupported Kimi OAuth token type")
	}
	return nil
}

// ValidateVerificationURL prevents an upstream response from linking admins to
// an unrelated site. Authorization/token endpoints themselves are never configurable.
func ValidateVerificationURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" &&
		(u.Host == "auth.kimi.com" || u.Host == "www.kimi.com" || u.Host == "kimi.com") && u.Fragment == ""
}

func SetDeviceHeaders(h http.Header, deviceID string) {
	// Kimi Code's public OAuth client uses this protocol platform identifier.
	h.Set("X-Msh-Platform", "kimi_cli")
	h.Set("X-Msh-Version", "1.0")
	h.Set("X-Msh-Device-Name", "Sub4API")
	h.Set("X-Msh-Device-Model", "server")
	h.Set("X-Msh-Device-Id", deviceID)
}
