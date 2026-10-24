package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadUpstreamClientIdleTTLDefaultAndOverride(t *testing.T) {
	for _, tc := range []struct {
		name, override string
		want           int
	}{{"default", "", 3600}, {"explicit-override", "900", 900}} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			configFile := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(configFile, []byte("{}\n"), 0o600))
			t.Setenv("CONFIG_FILE", configFile)
			t.Setenv("GATEWAY_CLIENT_IDLE_TTL_SECONDS", tc.override)
			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Gateway.ClientIdleTTLSeconds)
		})
	}
}
