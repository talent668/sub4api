package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageLogUpstreamServiceTierMigration(t *testing.T) {
	content, err := FS.ReadFile("247_usage_log_upstream_service_tier.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS upstream_service_tier VARCHAR(16)")
	require.Contains(t, sql, "COMMENT ON COLUMN usage_logs.upstream_service_tier")
}
