package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIFastBridgeSettings_DefaultsOffAndRoundTrip(t *testing.T) {
	repo := &openAIFastPolicyRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, nil)

	settings, err := svc.GetOpenAIFastBridgeSettings(context.Background())
	require.NoError(t, err)
	require.NotNil(t, settings)
	require.False(t, settings.Enabled)

	require.NoError(t, svc.SetOpenAIFastBridgeSettings(context.Background(), &OpenAIFastBridgeSettings{Enabled: true}))
	settings, err = svc.GetOpenAIFastBridgeSettings(context.Background())
	require.NoError(t, err)
	require.NotNil(t, settings)
	require.True(t, settings.Enabled)

	require.NoError(t, svc.SetOpenAIFastBridgeSettings(context.Background(), &OpenAIFastBridgeSettings{Enabled: false}))
	settings, err = svc.GetOpenAIFastBridgeSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
}

func TestOpenAIFastBridgeSettings_RejectsNil(t *testing.T) {
	svc := NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, nil)
	require.Error(t, svc.SetOpenAIFastBridgeSettings(context.Background(), nil))
}
