package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/MACOS-DO/sub4api/internal/server/middleware"
	"github.com/MACOS-DO/sub4api/internal/service"
	"github.com/gin-gonic/gin"
)

type testSettingRepo struct {
	values map[string]string
}

func newTestSettingRepo() *testSettingRepo {
	return &testSettingRepo{values: map[string]string{}}
}

func (s *testSettingRepo) Get(ctx context.Context, key string) (*service.Setting, error) {
	v, err := s.GetValue(ctx, key)
	if err != nil {
		return nil, err
	}
	return &service.Setting{Key: key, Value: v}, nil
}
func (s *testSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	v, ok := s.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return v, nil
}
func (s *testSettingRepo) Set(ctx context.Context, key, value string) error {
	s.values[key] = value
	return nil
}
func (s *testSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := s.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}
func (s *testSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	for k, v := range settings {
		s.values[k] = v
	}
	return nil
}
func (s *testSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}
func (s *testSettingRepo) Delete(ctx context.Context, key string) error {
	delete(s.values, key)
	return nil
}

func newOpsRuntimeRouter(handler *OpsHandler, withUser bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if withUser {
		r.Use(func(c *gin.Context) {
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
			c.Next()
		})
	}
	r.GET("/runtime/logging", handler.GetRuntimeLogConfig)
	r.PUT("/runtime/logging", handler.UpdateRuntimeLogConfig)
	r.POST("/runtime/logging/reset", handler.ResetRuntimeLogConfig)
	return r
}

func newRuntimeOpsService(t *testing.T) *service.OpsService {
	t.Helper()
	if err := logger.Init(logger.InitOptions{
		Level:       "info",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output: logger.OutputOptions{
			ToStdout: false,
			ToFile:   false,
		},
	}); err != nil {
		t.Fatalf("init logger: %v", err)
	}

	settingRepo := newTestSettingRepo()
	cfg := &config.Config{
		Ops: config.OpsConfig{Enabled: true},
		Log: config.LogConfig{
			Level:           "info",
			Caller:          true,
			StacktraceLevel: "error",
			Sampling: config.LogSamplingConfig{
				Enabled:    false,
				Initial:    100,
				Thereafter: 100,
			},
		},
	}
	return service.NewOpsService(nil, settingRepo, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
}

func TestOpsRuntimeLoggingHandler_DiagnosticRetentionPresenceAndValidation(t *testing.T) {
	h := NewOpsHandler(newRuntimeOpsService(t))
	r := newOpsRuntimeRouter(h, true)
	for _, tc := range []struct {
		name   string
		fields string
		status int
		days   int
		size   int
	}{
		{"old client defaults", "", http.StatusOK, 7, 0},
		{"explicit policy", `,"diagnostic_retention_days":21,"diagnostic_max_size_mb":128`, http.StatusOK, 21, 128},
		{"old client preserves", "", http.StatusOK, 21, 128},
		{"null preserves", `,"diagnostic_retention_days":null,"diagnostic_max_size_mb":null`, http.StatusOK, 21, 128},
		{"zero disables quota", `,"diagnostic_max_size_mb":0`, http.StatusOK, 21, 0},
		{"minimum days", `,"diagnostic_retention_days":1`, http.StatusOK, 1, 0},
		{"maximums", `,"diagnostic_retention_days":3650,"diagnostic_max_size_mb":102400`, http.StatusOK, 3650, 102400},
		{"zero days rejected", `,"diagnostic_retention_days":0`, http.StatusBadRequest, 0, 0},
		{"negative days rejected", `,"diagnostic_retention_days":-1`, http.StatusBadRequest, 0, 0},
		{"excess days rejected", `,"diagnostic_retention_days":3651`, http.StatusBadRequest, 0, 0},
		{"negative quota rejected", `,"diagnostic_max_size_mb":-1`, http.StatusBadRequest, 0, 0},
		{"excess quota rejected", `,"diagnostic_max_size_mb":102401`, http.StatusBadRequest, 0, 0},
		{"fraction rejected", `,"diagnostic_retention_days":1.5`, http.StatusBadRequest, 0, 0},
		{"string rejected", `,"diagnostic_max_size_mb":"1"`, http.StatusBadRequest, 0, 0},
		{"invalid writes preserve previous", "", http.StatusOK, 3650, 102400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"level":"info","sampling_initial":100,"sampling_thereafter":100,"stacktrace_level":"error","retention_days":30` + tc.fields + `}`
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/runtime/logging", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status=%d, want %d, body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.status != http.StatusOK {
				return
			}
			var result struct {
				Data service.OpsRuntimeLogConfig `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Data.DiagnosticRetentionDays != tc.days || result.Data.DiagnosticMaxSizeMB != tc.size || result.Data.RetentionDays != 30 {
				t.Fatalf("unexpected policy/system retention: %+v", result.Data)
			}
		})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/runtime/logging/reset", nil))
	var result struct {
		Data service.OpsRuntimeLogConfig `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || result.Data.DiagnosticRetentionDays != 7 || result.Data.DiagnosticMaxSizeMB != 0 {
		t.Fatalf("reset did not return7/0: status=%d body=%s err=%v", w.Code, w.Body.String(), err)
	}
}

func TestOpsRuntimeLoggingHandler_RequestDiagnosticsPresence(t *testing.T) {
	h := NewOpsHandler(newRuntimeOpsService(t))
	r := newOpsRuntimeRouter(h, true)
	for _, tc := range []struct {
		field string
		want  bool
	}{
		{field: `,"persist_request_diagnostics":true`, want: true},
		{field: "", want: true},
		{field: `,"persist_request_diagnostics":null`, want: true},
		{field: `,"persist_request_diagnostics":false`, want: false},
		{field: "", want: false},
	} {
		body := `{"level":"info","sampling_initial":100,"sampling_thereafter":100,"stacktrace_level":"error","retention_days":30` + tc.field + `}`
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/runtime/logging", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("update status=%d, body=%s", w.Code, w.Body.String())
		}
		var result struct {
			Data service.OpsRuntimeLogConfig `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Data.PersistRequestDiagnostics != tc.want {
			t.Fatalf("field %q diagnostic persistence = %v, want %v", tc.field, result.Data.PersistRequestDiagnostics, tc.want)
		}
	}
}

func TestOpsRuntimeLoggingHandler_GetConfig(t *testing.T) {
	h := NewOpsHandler(newRuntimeOpsService(t))
	r := newOpsRuntimeRouter(h, false)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runtime/logging", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
}

func TestOpsRuntimeLoggingHandler_UpdateUnauthorized(t *testing.T) {
	h := NewOpsHandler(newRuntimeOpsService(t))
	r := newOpsRuntimeRouter(h, false)

	body := `{"level":"debug","enable_sampling":false,"sampling_initial":100,"sampling_thereafter":100,"caller":true,"stacktrace_level":"error","retention_days":30}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/logging", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", w.Code)
	}
}

func TestOpsRuntimeLoggingHandler_UpdateAndResetSuccess(t *testing.T) {
	h := NewOpsHandler(newRuntimeOpsService(t))
	r := newOpsRuntimeRouter(h, true)

	payload := map[string]any{
		"level":               "debug",
		"enable_sampling":     false,
		"sampling_initial":    100,
		"sampling_thereafter": 100,
		"caller":              true,
		"stacktrace_level":    "error",
		"retention_days":      30,
	}
	raw, _ := json.Marshal(payload)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/logging", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update status=%d, want 200, body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/runtime/logging/reset", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reset status=%d, want 200, body=%s", w.Code, w.Body.String())
	}
}
