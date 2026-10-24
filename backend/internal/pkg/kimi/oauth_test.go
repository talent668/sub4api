package kimi

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestKimiVerificationURLAllowlist(t *testing.T) {
	for _, raw := range []string{"https://auth.kimi.com/device?user_code=ABCD", "https://www.kimi.com/code/authorize"} {
		require.True(t, ValidateVerificationURL(raw), raw)
	}
	for _, raw := range []string{"http://auth.kimi.com/device", "https://auth.kimi.com.evil.example/device", "https://evil.example/@auth.kimi.com", "https://user@auth.kimi.com/device", "https://auth.kimi.com:443/device", "javascript:alert(1)"} {
		require.False(t, ValidateVerificationURL(raw), raw)
	}
}

func TestKimiTokenValidation(t *testing.T) {
	require.Error(t, (&Token{AccessToken: "secret", ExpiresIn: 3600, TokenType: "unexpected"}).Validate())
	require.Error(t, (&Token{AccessToken: "secret", ExpiresIn: -1}).Validate())
	require.NoError(t, (&Token{AccessToken: "secret", ExpiresIn: 3600, TokenType: "Bearer"}).Validate())
}
