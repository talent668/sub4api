package repository

import (
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUsageTransportInsertTailIsNullableAndSanitized(t *testing.T) {
	log := &service.UsageLog{Model: "m"}
	prepared := prepareUsageLogInsert(log)
	require.Nil(t, prepared.args[len(prepared.args)-1])
	require.Equal(t, "jsonb", usageLogInsertArgTypes[len(usageLogInsertArgTypes)-1])
	log.TransportDetails = &service.UsageTransportDetails{Client: "ws", Upstream: "http", Mode: "http_bridge", FallbackReason: "private-secret"}
	prepared = prepareUsageLogInsert(log)
	value, ok := prepared.args[len(prepared.args)-1].(string)
	require.True(t, ok)
	require.NotContains(t, value, "private-secret")
	require.Equal(t, &service.UsageTransportDetails{Client: "ws", Upstream: "http", Mode: "http_bridge"}, service.ParseUsageTransportDetailsJSON(value))
}
