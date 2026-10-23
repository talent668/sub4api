package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIFastBridgeSettings_DefaultsAndLegacy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		enabled bool
	}{
		{name: "missing"},
		{name: "empty", raw: ""},
		{name: "legacy enabled", raw: `{"enabled":true}`, enabled: true},
		{name: "legacy disabled", raw: `{"enabled":false}`},
		{name: "empty object", raw: `{}`},
		{name: "null", raw: `null`},
		{name: "malformed", raw: `{`},
		{name: "invalid scope", raw: `{"enabled":true,"scope":"unknown"}`},
		{name: "wrong scope type", raw: `{"enabled":true,"scope":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &openAIFastPolicyRepoStub{values: map[string]string{}}
			if tc.name != "missing" {
				repo.values[SettingKeyOpenAIFastBridgeSettings] = tc.raw
			}
			svc := NewSettingService(repo, nil)
			settings, err := svc.GetOpenAIFastBridgeSettings(context.Background())
			require.NoError(t, err)
			require.Equal(t, &OpenAIFastBridgeSettings{Enabled: tc.enabled, Scope: OpenAIFastBridgeScopeAll}, settings)
			require.Equal(t, *settings, svc.GetOpenAIFastBridgeSettingsCached(context.Background()))
		})
	}
}

func TestOpenAIFastBridgeSettings_RoundTripAndLegacySave(t *testing.T) {
	repo := &openAIFastPolicyRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, nil)
	ctx := context.Background()
	for _, settings := range []OpenAIFastBridgeSettings{
		{Enabled: false, Scope: OpenAIFastBridgeScopeFastOnly},
		{Enabled: true, Scope: OpenAIFastBridgeScopeFastOnly},
		{Enabled: true, Scope: OpenAIFastBridgeScopeAll},
	} {
		require.NoError(t, svc.SetOpenAIFastBridgeSettings(ctx, &settings))
		got, err := svc.GetOpenAIFastBridgeSettings(ctx)
		require.NoError(t, err)
		require.Equal(t, settings, *got)
		require.Equal(t, settings, svc.GetOpenAIFastBridgeSettingsCached(ctx))
		var persisted OpenAIFastBridgeSettings
		require.NoError(t, json.Unmarshal([]byte(repo.values[SettingKeyOpenAIFastBridgeSettings]), &persisted))
		require.Equal(t, settings, persisted)
	}

	require.NoError(t, svc.SetOpenAIFastBridgeSettings(ctx, &OpenAIFastBridgeSettings{Enabled: true, Scope: OpenAIFastBridgeScopeFastOnly}))
	// Old Go callers omit scope; keep the stored fast_only value, even if the cache is stale.
	svc.storeOpenAIFastBridgeSettingsCache(OpenAIFastBridgeSettings{Enabled: true, Scope: OpenAIFastBridgeScopeAll}, time.Minute)
	require.NoError(t, svc.SetOpenAIFastBridgeSettings(ctx, &OpenAIFastBridgeSettings{Enabled: false}))
	require.Equal(t, OpenAIFastBridgeSettings{Enabled: false, Scope: OpenAIFastBridgeScopeFastOnly}, svc.GetOpenAIFastBridgeSettingsCached(ctx))

	fresh := NewSettingService(&openAIFastPolicyRepoStub{values: map[string]string{}}, nil)
	require.NoError(t, fresh.SetOpenAIFastBridgeSettings(ctx, &OpenAIFastBridgeSettings{Enabled: true}))
	require.Equal(t, OpenAIFastBridgeSettings{Enabled: true, Scope: OpenAIFastBridgeScopeAll}, fresh.GetOpenAIFastBridgeSettingsCached(ctx))
}

func TestOpenAIFastBridgeSettings_RejectsInvalid(t *testing.T) {
	repo := &openAIFastPolicyRepoStub{values: map[string]string{}}
	svc := NewSettingService(repo, nil)
	require.Error(t, svc.SetOpenAIFastBridgeSettings(context.Background(), nil))
	for _, scope := range []string{"unknown", "ALL", " all ", "fast"} {
		require.Error(t, svc.SetOpenAIFastBridgeSettings(context.Background(), &OpenAIFastBridgeSettings{Enabled: true, Scope: scope}))
	}
	require.Empty(t, repo.values)
}

func TestOpenAIFastBridgeSettings_CachedSnapshot(t *testing.T) {
	repo := &countingFastBridgeSettingRepo{openAIFastPolicyRepoStub: openAIFastPolicyRepoStub{values: map[string]string{
		SettingKeyOpenAIFastBridgeSettings: `{"enabled":true,"scope":"fast_only"}`,
	}}}
	svc := NewSettingService(repo, nil)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := svc.GetOpenAIFastBridgeSettingsCached(context.Background())
			if got != (OpenAIFastBridgeSettings{Enabled: true, Scope: OpenAIFastBridgeScopeFastOnly}) {
				t.Errorf("unexpected snapshot: %+v", got)
			}
		}()
	}
	wg.Wait()
	require.True(t, svc.IsOpenAIFastBridgeEnabledCached(context.Background()))
	require.Equal(t, int32(1), repo.bridgeReads.Load())
	// The API returns a copy, not a mutable cached pointer.
	got := svc.GetOpenAIFastBridgeSettingsCached(context.Background())
	got.Scope = OpenAIFastBridgeScopeAll
	require.Equal(t, OpenAIFastBridgeScopeFastOnly, svc.GetOpenAIFastBridgeSettingsCached(context.Background()).Scope)
	// Changing scope without changing enabled still refreshes the complete snapshot.
	require.NoError(t, svc.SetOpenAIFastBridgeSettings(context.Background(), &OpenAIFastBridgeSettings{Enabled: true, Scope: OpenAIFastBridgeScopeAll}))
	require.Equal(t, OpenAIFastBridgeSettings{Enabled: true, Scope: OpenAIFastBridgeScopeAll}, svc.GetOpenAIFastBridgeSettingsCached(context.Background()))
}

func TestOpenAIFastBridgeSettings_CacheFailureDefaultsOff(t *testing.T) {
	repo := &countingFastBridgeSettingRepo{bridgeErr: errors.New("db down")}
	svc := NewSettingService(repo, nil)
	for i := 0; i < 3; i++ {
		require.Equal(t, *DefaultOpenAIFastBridgeSettings(), svc.GetOpenAIFastBridgeSettingsCached(context.Background()))
	}
	require.Equal(t, int32(1), repo.bridgeReads.Load())
	var nilService *SettingService
	require.Equal(t, *DefaultOpenAIFastBridgeSettings(), nilService.GetOpenAIFastBridgeSettingsCached(context.Background()))
	require.False(t, nilService.IsOpenAIFastBridgeEnabledCached(context.Background()))
	require.Equal(t, *DefaultOpenAIFastBridgeSettings(), (&SettingService{}).GetOpenAIFastBridgeSettingsCached(context.Background()))
}

type blockedFastBridgeSettingsRepo struct {
	openAIFastPolicyRepoStub
	mu      sync.Mutex
	blocked atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (r *blockedFastBridgeSettingsRepo) GetValue(ctx context.Context, key string) (string, error) {
	r.mu.Lock()
	value, err := r.openAIFastPolicyRepoStub.GetValue(ctx, key)
	r.mu.Unlock()
	if r.blocked.CompareAndSwap(false, true) {
		close(r.started)
		<-r.release
	}
	return value, err
}

func (r *blockedFastBridgeSettingsRepo) Set(ctx context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.openAIFastPolicyRepoStub.Set(ctx, key, value)
}

func TestOpenAIFastBridgeSettings_ConcurrentPatchesPreserveBothFields(t *testing.T) {
	for _, scopeFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "scope first", false: "disable first"}[scopeFirst], func(t *testing.T) {
			repo := &blockedFastBridgeSettingsRepo{
				openAIFastPolicyRepoStub: openAIFastPolicyRepoStub{values: map[string]string{
					SettingKeyOpenAIFastBridgeSettings: `{"enabled":true,"scope":"all"}`,
				}},
				started: make(chan struct{}), release: make(chan struct{}),
			}
			svc := NewSettingService(repo, nil)
			disabled, scope := false, OpenAIFastBridgeScopeFastOnly
			patchScope := func() error {
				_, err := svc.PatchOpenAIFastBridgeSettings(context.Background(), nil, &scope)
				return err
			}
			patchEnabled := func() error {
				_, err := svc.PatchOpenAIFastBridgeSettings(context.Background(), &disabled, nil)
				return err
			}
			first, second := patchScope, patchEnabled
			if !scopeFirst {
				first, second = second, first
			}
			firstDone, secondDone := make(chan error, 1), make(chan error, 1)
			go func() { firstDone <- first() }()
			<-repo.started // First patch captured the old persisted record.
			go func() { secondDone <- second() }()
			select {
			case err := <-secondDone:
				close(repo.release)
				require.NoError(t, <-firstDone)
				require.NoError(t, err)
				t.Fatal("second partial update bypassed the first update's merge lock")
			case <-time.After(25 * time.Millisecond):
				close(repo.release)
			}
			require.NoError(t, <-firstDone)
			require.NoError(t, <-secondDone)
			want := OpenAIFastBridgeSettings{Enabled: false, Scope: OpenAIFastBridgeScopeFastOnly}
			got, err := svc.GetOpenAIFastBridgeSettings(context.Background())
			require.NoError(t, err)
			require.Equal(t, want, *got)
			require.Equal(t, want, svc.GetOpenAIFastBridgeSettingsCached(context.Background()))
		})
	}
}

func TestOpenAIFastBridgeSettings_OldReadCannotOverwriteSave(t *testing.T) {
	repo := &blockedFastBridgeSettingsRepo{
		openAIFastPolicyRepoStub: openAIFastPolicyRepoStub{values: map[string]string{
			SettingKeyOpenAIFastBridgeSettings: `{"enabled":false,"scope":"all"}`,
		}},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	svc := NewSettingService(repo, nil)
	readDone := make(chan struct{})
	go func() {
		svc.GetOpenAIFastBridgeSettingsCached(context.Background())
		close(readDone)
	}()
	<-repo.started // The reader has captured the old DB value but not published it.
	written := OpenAIFastBridgeSettings{Enabled: true, Scope: OpenAIFastBridgeScopeFastOnly}
	writeDone := make(chan error, 1)
	go func() { writeDone <- svc.SetOpenAIFastBridgeSettings(context.Background(), &written) }()

	// A serialized implementation waits; a generation-checked one may save now.
	// In either case, releasing the stale reader must not undo the saved snapshot.
	select {
	case err := <-writeDone:
		require.NoError(t, err)
		close(repo.release)
	case <-time.After(50 * time.Millisecond):
		close(repo.release)
		require.NoError(t, <-writeDone)
	}
	<-readDone
	require.Equal(t, written, svc.GetOpenAIFastBridgeSettingsCached(context.Background()))
}
