package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/handler/dto"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fastBridgeHandlerRepo struct {
	settingHandlerRepoStub
}

func (r *fastBridgeHandlerRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func newFastBridgeSettingsHandler(t *testing.T, raw string) (*SettingHandler, *fastBridgeHandlerRepo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &fastBridgeHandlerRepo{settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{}}}
	if raw != "" {
		repo.values[service.SettingKeyOpenAIFastBridgeSettings] = raw
	}
	return NewSettingHandler(service.NewSettingService(repo, nil), nil, nil, nil, nil, nil, nil), repo
}

func requestFastBridgeSettings(t *testing.T, handler *SettingHandler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, "/api/v1/admin/settings/openai-fast-bridge", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if method == http.MethodGet {
		handler.GetOpenAIFastBridgeSettings(c)
	} else {
		handler.UpdateOpenAIFastBridgeSettings(c)
	}
	return rec
}

func requireFastBridgeSettingsResponse(t *testing.T, rec *httptest.ResponseRecorder, enabled bool, scope string) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response struct {
		Data dto.OpenAIFastBridgeSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, dto.OpenAIFastBridgeSettings{Enabled: enabled, Scope: scope}, response.Data)
}

func TestSettingHandler_OpenAIFastBridgeDefaultsAndLegacy(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		enabled bool
	}{
		{},
		{raw: `{"enabled":false}`},
		{raw: `{"enabled":true}`, enabled: true},
	} {
		handler, _ := newFastBridgeSettingsHandler(t, tc.raw)
		requireFastBridgeSettingsResponse(t, requestFastBridgeSettings(t, handler, http.MethodGet, ""), tc.enabled, service.OpenAIFastBridgeScopeAll)
	}
}

func TestSettingHandler_OpenAIFastBridgeRoundTrip(t *testing.T) {
	handler, repo := newFastBridgeSettingsHandler(t, "")
	for _, tc := range []struct {
		body    string
		enabled bool
		scope   string
	}{
		{body: `{"enabled":false,"scope":"fast_only"}`, scope: service.OpenAIFastBridgeScopeFastOnly},
		{body: `{"enabled":true,"scope":"fast_only"}`, enabled: true, scope: service.OpenAIFastBridgeScopeFastOnly},
		{body: `{"enabled":true,"scope":"all"}`, enabled: true, scope: service.OpenAIFastBridgeScopeAll},
	} {
		requireFastBridgeSettingsResponse(t, requestFastBridgeSettings(t, handler, http.MethodPut, tc.body), tc.enabled, tc.scope)
		requireFastBridgeSettingsResponse(t, requestFastBridgeSettings(t, handler, http.MethodGet, ""), tc.enabled, tc.scope)
		require.JSONEq(t, tc.body, repo.values[service.SettingKeyOpenAIFastBridgeSettings])
	}
}

func TestSettingHandler_OpenAIFastBridgeScopeOnlyPreservesEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		raw, err := json.Marshal(dto.OpenAIFastBridgeSettings{Enabled: enabled, Scope: service.OpenAIFastBridgeScopeAll})
		require.NoError(t, err)
		handler, _ := newFastBridgeSettingsHandler(t, string(raw))
		requireFastBridgeSettingsResponse(t, requestFastBridgeSettings(t, handler, http.MethodPut, `{"scope":"fast_only"}`), enabled, service.OpenAIFastBridgeScopeFastOnly)
		requireFastBridgeSettingsResponse(t, requestFastBridgeSettings(t, handler, http.MethodPut, `{"scope":"all"}`), enabled, service.OpenAIFastBridgeScopeAll)
	}
}

func TestSettingHandler_OpenAIFastBridgeLegacyPutPreservesScope(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		scope string
	}{
		{scope: service.OpenAIFastBridgeScopeAll},
		{raw: `{"enabled":false}`, scope: service.OpenAIFastBridgeScopeAll},
		{raw: `{"enabled":false,"scope":"all"}`, scope: service.OpenAIFastBridgeScopeAll},
		{raw: `{"enabled":false,"scope":"fast_only"}`, scope: service.OpenAIFastBridgeScopeFastOnly},
	} {
		handler, _ := newFastBridgeSettingsHandler(t, tc.raw)
		requireFastBridgeSettingsResponse(t, requestFastBridgeSettings(t, handler, http.MethodPut, `{"enabled":true}`), true, tc.scope)
		requireFastBridgeSettingsResponse(t, requestFastBridgeSettings(t, handler, http.MethodPut, `{"enabled":false}`), false, tc.scope)
	}
}

type blockingFastBridgeHandlerRepo struct {
	fastBridgeHandlerRepo
	mu      sync.Mutex
	blocked atomic.Bool
	started chan struct{}
	release chan struct{}
}

func (r *blockingFastBridgeHandlerRepo) GetValue(ctx context.Context, key string) (string, error) {
	r.mu.Lock()
	value, err := r.fastBridgeHandlerRepo.GetValue(ctx, key)
	r.mu.Unlock()
	if r.blocked.CompareAndSwap(false, true) {
		close(r.started)
		<-r.release
	}
	return value, err
}

func (r *blockingFastBridgeHandlerRepo) Set(ctx context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fastBridgeHandlerRepo.Set(ctx, key, value)
}

func TestSettingHandler_OpenAIFastBridgeConcurrentPartialPuts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &blockingFastBridgeHandlerRepo{
		fastBridgeHandlerRepo: fastBridgeHandlerRepo{settingHandlerRepoStub: settingHandlerRepoStub{values: map[string]string{
			service.SettingKeyOpenAIFastBridgeSettings: `{"enabled":true,"scope":"all"}`,
		}}},
		started: make(chan struct{}), release: make(chan struct{}),
	}
	handler := NewSettingHandler(service.NewSettingService(repo, nil), nil, nil, nil, nil, nil, nil)
	scopeDone, disableDone := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() {
		scopeDone <- requestFastBridgeSettings(t, handler, http.MethodPut, `{"scope":"fast_only"}`)
	}()
	<-repo.started // Scope-only request has read enabled:true, but must hold the merge lock.
	go func() {
		disableDone <- requestFastBridgeSettings(t, handler, http.MethodPut, `{"enabled":false}`)
	}()
	select {
	case rec := <-disableDone:
		close(repo.release)
		<-scopeDone
		require.Equal(t, http.StatusOK, rec.Code)
		t.Fatal("enabled-only PUT bypassed the in-flight scope merge")
	case <-time.After(25 * time.Millisecond):
		close(repo.release)
	}
	requireFastBridgeSettingsResponse(t, <-scopeDone, true, service.OpenAIFastBridgeScopeFastOnly)
	requireFastBridgeSettingsResponse(t, <-disableDone, false, service.OpenAIFastBridgeScopeFastOnly)
	requireFastBridgeSettingsResponse(t, requestFastBridgeSettings(t, handler, http.MethodGet, ""), false, service.OpenAIFastBridgeScopeFastOnly)
}

func TestSettingHandler_OpenAIFastBridgeRejectsInvalidScope(t *testing.T) {
	for _, scope := range []string{`""`, `null`, `"unknown"`, `"ALL"`, `" all "`, `true`, `1`, `{}`, `[]`} {
		t.Run(scope, func(t *testing.T) {
			const original = `{"enabled":true,"scope":"fast_only"}`
			handler, repo := newFastBridgeSettingsHandler(t, original)
			rec := requestFastBridgeSettings(t, handler, http.MethodPut, `{"enabled":false,"scope":`+scope+`}`)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Equal(t, original, repo.values[service.SettingKeyOpenAIFastBridgeSettings])
		})
	}
}
