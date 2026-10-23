package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MACOS-DO/sub4api/internal/config"
	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
)

type runtimeSettingRepoStub struct {
	values           map[string]string
	deleted          map[string]bool
	setCalls         int
	getValueCalls    int
	getMultipleCalls int
	getValueFn       func(key string) (string, error)
	setFn            func(key, value string) error
	deleteFn         func(key string) error
}

type runtimeCleanupReloader struct {
	calls int
}

func (r *runtimeCleanupReloader) Reload(context.Context) error {
	r.calls++
	return nil
}

func newRuntimeSettingRepoStub() *runtimeSettingRepoStub {
	return &runtimeSettingRepoStub{
		values:  map[string]string{},
		deleted: map[string]bool{},
	}
}

func (s *runtimeSettingRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	value, err := s.GetValue(ctx, key)
	if err != nil {
		return nil, err
	}
	return &Setting{Key: key, Value: value}, nil
}

func (s *runtimeSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	s.getValueCalls++
	if s.getValueFn != nil {
		return s.getValueFn(key)
	}
	value, ok := s.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (s *runtimeSettingRepoStub) Set(_ context.Context, key, value string) error {
	if s.setFn != nil {
		if err := s.setFn(key, value); err != nil {
			return err
		}
	}
	s.values[key] = value
	s.setCalls++
	return nil
}

func (s *runtimeSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	s.getMultipleCalls++
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := s.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (s *runtimeSettingRepoStub) SetMultiple(_ context.Context, settings map[string]string) error {
	for key, value := range settings {
		s.values[key] = value
	}
	return nil
}

func (s *runtimeSettingRepoStub) GetAll(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(s.values))
	for key, value := range s.values {
		out[key] = value
	}
	return out, nil
}

func (s *runtimeSettingRepoStub) Delete(_ context.Context, key string) error {
	if s.deleteFn != nil {
		if err := s.deleteFn(key); err != nil {
			return err
		}
	}
	if _, ok := s.values[key]; !ok {
		return ErrSettingNotFound
	}
	delete(s.values, key)
	s.deleted[key] = true
	return nil
}

func TestRuntimeLogConfig_DiagnosticRetentionDefaultsAndValidation(t *testing.T) {
	defaults := defaultOpsRuntimeLogConfig(nil)
	if defaults.DiagnosticRetentionDays != 7 || defaults.DiagnosticMaxSizeMB != 0 || defaults.RetentionDays != 30 {
		t.Fatalf("unexpected diagnostic/system defaults: %+v", defaults)
	}
	for _, raw := range []string{`{}`, `{"level":"info"}`, `{"diagnostic_retention_days":null,"diagnostic_max_size_mb":null}`} {
		cfg, valid := decodeOpsRuntimeLogConfig(raw, defaults)
		if !valid || cfg.DiagnosticRetentionDays != 7 || cfg.DiagnosticMaxSizeMB != 0 {
			t.Fatalf("older JSON %q lost defaults: %+v valid=%v", raw, cfg, valid)
		}
	}
	for _, tc := range []struct{ days, size int }{{1, 0}, {3650, 102400}, {7, 1}} {
		cfg := *defaults
		cfg.DiagnosticRetentionDays, cfg.DiagnosticMaxSizeMB = tc.days, tc.size
		if err := validateOpsRuntimeLogConfig(&cfg); err != nil {
			t.Fatalf("valid policy %+v rejected: %v", tc, err)
		}
	}
	for _, tc := range []struct{ days, size int }{{0, 0}, {-1, 0}, {3651, 0}, {7, -1}, {7, 102401}} {
		cfg := *defaults
		cfg.DiagnosticRetentionDays, cfg.DiagnosticMaxSizeMB = tc.days, tc.size
		normalizeOpsRuntimeLogConfig(&cfg, defaults)
		if err := validateOpsRuntimeLogConfig(&cfg); err == nil {
			t.Fatalf("explicit invalid policy %+v was normalized/accepted", tc)
		}
	}
}

func TestRuntimeLogConfig_DiagnosticRetentionPublicationAfterPersistence(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	old := defaultOpsRuntimeLogConfig(nil)
	old.DiagnosticRetentionDays, old.DiagnosticMaxSizeMB = 21, 256
	raw, _ := json.Marshal(old)
	repo.values[SettingKeyOpsRuntimeLogConfig] = string(raw)
	sink := &OpsSystemLogSink{}
	svc := &OpsService{settingRepo: repo, systemLogSink: sink}
	svc.applyRuntimeLogConfigOnStartup(context.Background())
	oldPolicy := RequestDiagnosticRetentionPolicy{RetentionDays: 21, MaxSizeMB: 256}
	if got := sink.diagnosticRetentionPolicy.Load(); got == nil || *got != oldPolicy {
		t.Fatalf("startup policy = %v, want %+v", got, oldPolicy)
	}
	next := *old
	next.DiagnosticRetentionDays, next.DiagnosticMaxSizeMB = 1, 1
	persistErr := errors.New("persistence unavailable")
	repo.setFn = func(string, string) error {
		if got := sink.diagnosticRetentionPolicy.Load(); got == nil || *got != oldPolicy {
			t.Fatalf("destructive policy published before Set: %v", got)
		}
		return persistErr
	}
	if _, err := svc.UpdateRuntimeLogConfig(context.Background(), &next, 7); !errors.Is(err, persistErr) {
		t.Fatalf("update error = %v", err)
	}
	if got := *sink.diagnosticRetentionPolicy.Load(); got != oldPolicy {
		t.Fatalf("failed update changed policy: %+v", got)
	}
	repo.setFn = nil
	if _, err := svc.UpdateRuntimeLogConfig(context.Background(), &next, 7); err != nil {
		t.Fatal(err)
	}
	nextPolicy := RequestDiagnosticRetentionPolicy{RetentionDays: 1, MaxSizeMB: 1}
	if got := *sink.diagnosticRetentionPolicy.Load(); got != nextPolicy {
		t.Fatalf("successful update did not publish: %+v", got)
	}
	repo.deleteFn = func(string) error {
		if got := *sink.diagnosticRetentionPolicy.Load(); got != nextPolicy {
			t.Fatalf("reset policy published before Delete: %+v", got)
		}
		return persistErr
	}
	if _, err := svc.ResetRuntimeLogConfig(context.Background(), 7); !errors.Is(err, persistErr) {
		t.Fatalf("reset error = %v", err)
	}
	if got := *sink.diagnosticRetentionPolicy.Load(); got != nextPolicy {
		t.Fatalf("failed reset changed policy: %+v", got)
	}
	repo.deleteFn = nil
	if _, err := svc.ResetRuntimeLogConfig(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if got := *sink.diagnosticRetentionPolicy.Load(); got != (RequestDiagnosticRetentionPolicy{RetentionDays: 7}) {
		t.Fatalf("successful reset did not publish defaults: %+v", got)
	}
}

func TestRuntimeLogConfig_DiagnosticRetentionInvalidLoadFailClosed(t *testing.T) {
	for _, raw := range []string{`{bad-json}`, `null`, `{"diagnostic_retention_days":0}`, `{"diagnostic_retention_days":3651}`, `{"diagnostic_max_size_mb":-1}`, `{"diagnostic_max_size_mb":102401}`} {
		t.Run(raw, func(t *testing.T) {
			repo := newRuntimeSettingRepoStub()
			repo.values[SettingKeyOpsRuntimeLogConfig] = raw
			sink := &OpsSystemLogSink{}
			svc := &OpsService{settingRepo: repo, systemLogSink: sink}
			svc.applyRuntimeLogConfigOnStartup(context.Background())
			if got := sink.diagnosticRetentionPolicy.Load(); got != nil {
				t.Fatalf("invalid startup authorized deletion: %+v", got)
			}
			if err := svc.RefreshRuntimeSettings(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := sink.diagnosticRetentionPolicy.Load(); got != nil {
				t.Fatalf("invalid refresh authorized deletion: %+v", got)
			}
			policy := RequestDiagnosticRetentionPolicy{RetentionDays: 21, MaxSizeMB: 128}
			if err := sink.SetRequestDiagnosticRetentionPolicy(policy); err != nil {
				t.Fatal(err)
			}
			if err := svc.RefreshRuntimeSettings(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := *sink.diagnosticRetentionPolicy.Load(); got != policy {
				t.Fatalf("invalid refresh replaced previous valid policy: %+v", got)
			}
		})
	}
}

func TestRefreshRuntimeSettings_DiagnosticRetentionLoadsWithoutCaptureOrMonitoring(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	repo.values[SettingKeyOpsMonitoringEnabled] = "false"
	repo.values[SettingKeyOpsAdvancedSettings] = `{"data_retention":{"cleanup_enabled":false}}`
	sink := &OpsSystemLogSink{}
	svc := &OpsService{settingRepo: repo, systemLogSink: sink}
	for _, tc := range []struct {
		raw  string
		want RequestDiagnosticRetentionPolicy
	}{
		{`{}`, RequestDiagnosticRetentionPolicy{RetentionDays: 7}},
		{`{"diagnostic_retention_days":14,"diagnostic_max_size_mb":32,"persist_request_diagnostics":false}`, RequestDiagnosticRetentionPolicy{RetentionDays: 14, MaxSizeMB: 32}},
	} {
		repo.values[SettingKeyOpsRuntimeLogConfig] = tc.raw
		if err := svc.RefreshRuntimeSettings(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := sink.diagnosticRetentionPolicy.Load(); got == nil || *got != tc.want {
			t.Fatalf("policy = %v, want %+v", got, tc.want)
		}
		if sink.RequestDiagnosticsEnabled() || svc.IsMonitoringEnabled(context.Background()) {
			t.Fatal("test expected capture and monitoring disabled")
		}
	}
	delete(repo.values, SettingKeyOpsRuntimeLogConfig)
	if err := svc.RefreshRuntimeSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := *sink.diagnosticRetentionPolicy.Load(); got != (RequestDiagnosticRetentionPolicy{RetentionDays: 7}) {
		t.Fatalf("missing stored config should publish defaults: %+v", got)
	}
}

func TestRuntimeLogConfig_RequestDiagnosticsDefaultsAndLifecycle(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	sink := &OpsSystemLogSink{}
	svc := &OpsService{settingRepo: repo, systemLogSink: sink}
	// Stored configurations written before this switch existed must remain off.
	repo.values[SettingKeyOpsRuntimeLogConfig] = `{"level":"info","extra":{"future_setting":"keep"}}`
	cfg, err := svc.GetRuntimeLogConfig(context.Background())
	if err != nil || cfg.PersistRequestDiagnostics || defaultOpsRuntimeLogConfig(nil).PersistRequestDiagnostics {
		t.Fatalf("diagnostics must default off: %#v, %v", cfg, err)
	}
	cfg.PersistRequestDiagnostics = true
	updated, err := svc.UpdateRuntimeLogConfig(context.Background(), cfg, 1)
	if err != nil || !updated.PersistRequestDiagnostics || !sink.RequestDiagnosticsEnabled() {
		t.Fatalf("update did not enable diagnostics: %#v, %v", updated, err)
	}
	// Omitted Extra from old clients must not erase unrelated stored settings.
	cfg.Extra = nil
	if _, err := svc.UpdateRuntimeLogConfig(context.Background(), cfg, 1); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.GetRuntimeLogConfig(context.Background())
	if err != nil || stored.Extra["future_setting"] != "keep" || !stored.PersistRequestDiagnostics {
		t.Fatalf("stored config lost settings: %#v, %v", stored, err)
	}
	sink.SetPersistRequestDiagnostics(false)
	svc.applyRuntimeLogConfigOnStartup(context.Background())
	if !sink.RequestDiagnosticsEnabled() {
		t.Fatal("startup did not apply diagnostic setting")
	}
	repo.setFn = func(string, string) error { return errors.New("write failed") }
	next := *stored
	next.PersistRequestDiagnostics = false
	if _, err := svc.UpdateRuntimeLogConfig(context.Background(), &next, 1); err == nil || !sink.RequestDiagnosticsEnabled() {
		t.Fatal("failed persistence did not restore enabled diagnostic gate")
	}
	repo.setFn = nil
	repo.deleteFn = func(string) error { return errors.New("delete failed") }
	if _, err := svc.ResetRuntimeLogConfig(context.Background(), 1); err == nil || !sink.RequestDiagnosticsEnabled() {
		t.Fatal("failed reset did not restore enabled diagnostic gate")
	}
	repo.deleteFn = nil
	reset, err := svc.ResetRuntimeLogConfig(context.Background(), 1)
	if err != nil || reset.PersistRequestDiagnostics || sink.RequestDiagnosticsEnabled() {
		t.Fatalf("reset did not disable diagnostics: %#v, %v", reset, err)
	}
}

func TestRefreshRuntimeSettings_RequestDiagnosticGate(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	sink := &OpsSystemLogSink{}
	svc := &OpsService{settingRepo: repo, systemLogSink: sink}
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{raw: `{"persist_request_diagnostics":true}`, want: true},
		{raw: `{"persist_access_logs":true}`, want: false},
		{raw: `{"persist_request_diagnostics":true}`, want: true},
		{raw: `{bad-json}`, want: false},
	} {
		repo.values[SettingKeyOpsRuntimeLogConfig] = tc.raw
		if err := svc.RefreshRuntimeSettings(context.Background()); err != nil {
			t.Fatal(err)
		}
		if sink.RequestDiagnosticsEnabled() != tc.want {
			t.Fatalf("refresh %q diagnostic gate = %v, want %v", tc.raw, sink.RequestDiagnosticsEnabled(), tc.want)
		}
	}
}

func TestRuntimeLogConfig_MarshalFailureDoesNotChangeDiagnosticGate(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	sink := &OpsSystemLogSink{}
	svc := &OpsService{settingRepo: repo, systemLogSink: sink}
	cfg := defaultOpsRuntimeLogConfig(nil)
	cfg.PersistRequestDiagnostics = true
	cfg.Extra = map[string]any{"unsupported": make(chan int)}
	if _, err := svc.UpdateRuntimeLogConfig(context.Background(), cfg, 1); err == nil || sink.RequestDiagnosticsEnabled() {
		t.Fatal("encoding failure changed the diagnostic gate")
	}
}

func TestUpdateRuntimeLogConfig_InvalidConfigShouldNotApply(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	svc := &OpsService{
		settingRepo: repo,
		cfg: &config.Config{
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
		},
	}

	if err := logger.Init(logger.InitOptions{
		Level:       "info",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output: logger.OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
	}); err != nil {
		t.Fatalf("init logger: %v", err)
	}

	_, err := svc.UpdateRuntimeLogConfig(context.Background(), &OpsRuntimeLogConfig{
		Level:           "trace",
		EnableSampling:  true,
		SamplingInitial: 100,
		SamplingNext:    100,
		Caller:          true,
		StacktraceLevel: "error",
		RetentionDays:   30,
	}, 1)
	if err == nil {
		t.Fatalf("expected validation error")
	}
	if logger.CurrentLevel() != "info" {
		t.Fatalf("logger level changed unexpectedly: %s", logger.CurrentLevel())
	}
	if repo.setCalls != 1 {
		// GetRuntimeLogConfig() 会在 key 缺失时写入默认值，此处应只有这一次持久化。
		t.Fatalf("unexpected set calls: %d", repo.setCalls)
	}
}

func TestResetRuntimeLogConfig_ShouldFallbackToBaseline(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	existing := &OpsRuntimeLogConfig{
		Level:           "debug",
		EnableSampling:  true,
		SamplingInitial: 50,
		SamplingNext:    50,
		Caller:          true,
		StacktraceLevel: "error",
		RetentionDays:   60,
		Source:          "runtime_setting",
	}
	raw, _ := json.Marshal(existing)
	repo.values[SettingKeyOpsRuntimeLogConfig] = string(raw)

	svc := &OpsService{
		settingRepo: repo,
		cfg: &config.Config{
			Log: config.LogConfig{
				Level:           "warn",
				Caller:          false,
				StacktraceLevel: "fatal",
				Sampling: config.LogSamplingConfig{
					Enabled:    false,
					Initial:    100,
					Thereafter: 100,
				},
			},
			Ops: config.OpsConfig{
				Cleanup: config.OpsCleanupConfig{
					ErrorLogRetentionDays:  7,
					SystemLogRetentionDays: 45,
				},
			},
		},
	}

	if err := logger.Init(logger.InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output: logger.OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
	}); err != nil {
		t.Fatalf("init logger: %v", err)
	}

	resetCfg, err := svc.ResetRuntimeLogConfig(context.Background(), 9)
	if err != nil {
		t.Fatalf("ResetRuntimeLogConfig() error: %v", err)
	}
	if resetCfg.Source != "baseline" {
		t.Fatalf("source = %q, want baseline", resetCfg.Source)
	}
	if resetCfg.Level != "warn" {
		t.Fatalf("level = %q, want warn", resetCfg.Level)
	}
	if resetCfg.RetentionDays != 45 {
		t.Fatalf("retention_days = %d, want 45", resetCfg.RetentionDays)
	}
	if logger.CurrentLevel() != "warn" {
		t.Fatalf("logger level = %q, want warn", logger.CurrentLevel())
	}
	if !repo.deleted[SettingKeyOpsRuntimeLogConfig] {
		t.Fatalf("runtime setting key should be deleted")
	}
}

func TestResetRuntimeLogConfig_InvalidOperator(t *testing.T) {
	svc := &OpsService{settingRepo: newRuntimeSettingRepoStub()}
	_, err := svc.ResetRuntimeLogConfig(context.Background(), 0)
	if err == nil {
		t.Fatalf("expected invalid operator error")
	}
	if err.Error() != "invalid operator id" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetRuntimeLogConfig_InvalidJSONFallback(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	repo.values[SettingKeyOpsRuntimeLogConfig] = `{invalid-json}`

	svc := &OpsService{
		settingRepo: repo,
		cfg: &config.Config{
			Log: config.LogConfig{
				Level:           "warn",
				Caller:          true,
				StacktraceLevel: "error",
				Sampling: config.LogSamplingConfig{
					Enabled:    false,
					Initial:    100,
					Thereafter: 100,
				},
			},
		},
	}

	got, err := svc.GetRuntimeLogConfig(context.Background())
	if err != nil {
		t.Fatalf("GetRuntimeLogConfig() error: %v", err)
	}
	if got.Level != "warn" {
		t.Fatalf("level = %q, want warn", got.Level)
	}
}

func TestUpdateRuntimeLogConfig_PersistFailureRollback(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	oldCfg := &OpsRuntimeLogConfig{
		Level:           "info",
		EnableSampling:  false,
		SamplingInitial: 100,
		SamplingNext:    100,
		Caller:          true,
		StacktraceLevel: "error",
		RetentionDays:   30,
	}
	raw, _ := json.Marshal(oldCfg)
	repo.values[SettingKeyOpsRuntimeLogConfig] = string(raw)
	repo.setFn = func(key, value string) error {
		if key == SettingKeyOpsRuntimeLogConfig {
			return errors.New("db down")
		}
		return nil
	}

	svc := &OpsService{
		settingRepo:   repo,
		systemLogSink: &OpsSystemLogSink{},
		cfg: &config.Config{
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
		},
	}

	if err := logger.Init(logger.InitOptions{
		Level:       "info",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output: logger.OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
	}); err != nil {
		t.Fatalf("init logger: %v", err)
	}

	_, err := svc.UpdateRuntimeLogConfig(context.Background(), &OpsRuntimeLogConfig{
		Level:                     "debug",
		PersistAccessLogs:         true,
		PersistRequestDiagnostics: true,
		EnableSampling:            false,
		SamplingInitial:           100,
		SamplingNext:              100,
		Caller:                    true,
		StacktraceLevel:           "error",
		RetentionDays:             30,
		DiagnosticRetentionDays:   7,
	}, 5)
	if err == nil {
		t.Fatalf("expected persist error")
	}
	// Persist failure should rollback runtime level back to old effective level.
	if logger.CurrentLevel() != "info" {
		t.Fatalf("logger level should rollback to info, got %s", logger.CurrentLevel())
	}
	if svc.systemLogSink.persistAccessLogs.Load() {
		t.Fatal("access-log persistence should rollback after setting write failure")
	}
	if svc.systemLogSink.RequestDiagnosticsEnabled() {
		t.Fatal("diagnostic persistence should rollback after setting write failure")
	}
}

func TestApplyRuntimeLogConfigOnStartup(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	cfgRaw := `{"level":"debug","persist_access_logs":true,"enable_sampling":false,"sampling_initial":100,"sampling_thereafter":100,"caller":true,"stacktrace_level":"error","retention_days":30}`
	repo.values[SettingKeyOpsRuntimeLogConfig] = cfgRaw

	svc := &OpsService{
		settingRepo:   repo,
		systemLogSink: &OpsSystemLogSink{},
		cfg: &config.Config{
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
		},
	}

	if err := logger.Init(logger.InitOptions{
		Level:       "info",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output: logger.OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
	}); err != nil {
		t.Fatalf("init logger: %v", err)
	}

	svc.applyRuntimeLogConfigOnStartup(context.Background())
	if logger.CurrentLevel() != "debug" {
		t.Fatalf("expected startup apply debug, got %s", logger.CurrentLevel())
	}
	if !svc.systemLogSink.persistAccessLogs.Load() {
		t.Fatal("startup config did not enable access-log persistence")
	}
}

func TestDefaultNormalizeAndValidateRuntimeLogConfig(t *testing.T) {
	defaults := defaultOpsRuntimeLogConfig(&config.Config{
		Log: config.LogConfig{
			Level:           "DEBUG",
			Caller:          false,
			StacktraceLevel: "FATAL",
			Sampling: config.LogSamplingConfig{
				Enabled:    true,
				Initial:    50,
				Thereafter: 20,
			},
		},
		Ops: config.OpsConfig{
			Cleanup: config.OpsCleanupConfig{
				ErrorLogRetentionDays:  7,
				SystemLogRetentionDays: 14,
			},
		},
	})
	if defaults.Level != "debug" || defaults.StacktraceLevel != "fatal" || defaults.RetentionDays != 14 {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}
	if defaults.PersistAccessLogs {
		t.Fatal("access-log persistence should be disabled by default")
	}

	cfg := &OpsRuntimeLogConfig{
		Level:                   " ",
		EnableSampling:          true,
		SamplingInitial:         0,
		SamplingNext:            -1,
		Caller:                  true,
		StacktraceLevel:         "",
		RetentionDays:           0,
		DiagnosticRetentionDays: 7,
	}
	normalizeOpsRuntimeLogConfig(cfg, defaults)
	if cfg.Level != "debug" || cfg.StacktraceLevel != "fatal" {
		t.Fatalf("normalize level/stacktrace failed: %+v", cfg)
	}
	if cfg.SamplingInitial != 50 || cfg.SamplingNext != 20 || cfg.RetentionDays != 14 {
		t.Fatalf("normalize numeric defaults failed: %+v", cfg)
	}
	if err := validateOpsRuntimeLogConfig(cfg); err != nil {
		t.Fatalf("validate normalized config should pass: %v", err)
	}
}

func TestValidateRuntimeLogConfigErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  *OpsRuntimeLogConfig
	}{
		{name: "nil", cfg: nil},
		{name: "bad level", cfg: &OpsRuntimeLogConfig{Level: "trace", StacktraceLevel: "error", SamplingInitial: 1, SamplingNext: 1, RetentionDays: 1}},
		{name: "bad stack", cfg: &OpsRuntimeLogConfig{Level: "info", StacktraceLevel: "warn", SamplingInitial: 1, SamplingNext: 1, RetentionDays: 1}},
		{name: "bad initial", cfg: &OpsRuntimeLogConfig{Level: "info", StacktraceLevel: "error", SamplingInitial: 0, SamplingNext: 1, RetentionDays: 1}},
		{name: "bad next", cfg: &OpsRuntimeLogConfig{Level: "info", StacktraceLevel: "error", SamplingInitial: 1, SamplingNext: 0, RetentionDays: 1}},
		{name: "bad retention", cfg: &OpsRuntimeLogConfig{Level: "info", StacktraceLevel: "error", SamplingInitial: 1, SamplingNext: 1, RetentionDays: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateOpsRuntimeLogConfig(tc.cfg); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestGetRuntimeLogConfigFallbackAndErrors(t *testing.T) {
	var nilSvc *OpsService
	cfg, err := nilSvc.GetRuntimeLogConfig(context.Background())
	if err != nil {
		t.Fatalf("nil svc should fallback default: %v", err)
	}
	if cfg.Level != "info" {
		t.Fatalf("unexpected nil svc default level: %s", cfg.Level)
	}

	repo := newRuntimeSettingRepoStub()
	repo.getValueFn = func(key string) (string, error) {
		return "", errors.New("boom")
	}
	svc := &OpsService{
		settingRepo: repo,
		cfg: &config.Config{
			Log: config.LogConfig{
				Level:           "warn",
				Caller:          true,
				StacktraceLevel: "error",
				Sampling: config.LogSamplingConfig{
					Enabled:    false,
					Initial:    100,
					Thereafter: 100,
				},
			},
		},
	}
	if _, err := svc.GetRuntimeLogConfig(context.Background()); err == nil {
		t.Fatalf("expected get value error")
	}
}

func TestUpdateRuntimeLogConfig_PreconditionErrors(t *testing.T) {
	svc := &OpsService{}
	if _, err := svc.UpdateRuntimeLogConfig(context.Background(), &OpsRuntimeLogConfig{}, 1); err == nil {
		t.Fatalf("expected setting repo not initialized")
	}

	svc = &OpsService{settingRepo: newRuntimeSettingRepoStub()}
	if _, err := svc.UpdateRuntimeLogConfig(context.Background(), nil, 1); err == nil {
		t.Fatalf("expected invalid config")
	}
	if _, err := svc.UpdateRuntimeLogConfig(context.Background(), &OpsRuntimeLogConfig{
		Level:           "info",
		StacktraceLevel: "error",
		SamplingInitial: 1,
		SamplingNext:    1,
		RetentionDays:   1,
	}, 0); err == nil {
		t.Fatalf("expected invalid operator")
	}
}

func TestUpdateRuntimeLogConfig_Success(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	reloader := &runtimeCleanupReloader{}
	svc := &OpsService{
		settingRepo:     repo,
		systemLogSink:   &OpsSystemLogSink{},
		cleanupReloader: reloader,
		cfg: &config.Config{
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
		},
	}

	if err := logger.Init(logger.InitOptions{
		Level:       "info",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output: logger.OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
	}); err != nil {
		t.Fatalf("init logger: %v", err)
	}

	next, err := svc.UpdateRuntimeLogConfig(context.Background(), &OpsRuntimeLogConfig{
		Level:                     "debug",
		PersistAccessLogs:         true,
		PersistRequestDiagnostics: true,
		EnableSampling:            false,
		SamplingInitial:           100,
		SamplingNext:              100,
		Caller:                    true,
		StacktraceLevel:           "error",
		RetentionDays:             30,
		DiagnosticRetentionDays:   7,
	}, 2)
	if err != nil {
		t.Fatalf("UpdateRuntimeLogConfig() error: %v", err)
	}
	if next.Source != "runtime_setting" || next.UpdatedByUserID != 2 || next.UpdatedAt == "" {
		t.Fatalf("unexpected metadata: %+v", next)
	}
	if logger.CurrentLevel() != "debug" {
		t.Fatalf("expected applied level debug, got %s", logger.CurrentLevel())
	}
	if !svc.systemLogSink.persistAccessLogs.Load() {
		t.Fatal("runtime update did not enable access-log persistence")
	}
	if reloader.calls != 1 {
		t.Fatalf("cleanup reload calls = %d, want 1", reloader.calls)
	}
}

func TestResetRuntimeLogConfig_IgnoreNotFoundDelete(t *testing.T) {
	repo := newRuntimeSettingRepoStub()
	repo.deleteFn = func(key string) error { return ErrSettingNotFound }
	svc := &OpsService{
		settingRepo: repo,
		cfg: &config.Config{
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
		},
	}
	if _, err := svc.ResetRuntimeLogConfig(context.Background(), 1); err != nil {
		t.Fatalf("reset should ignore ErrSettingNotFound: %v", err)
	}
}

func TestApplyRuntimeLogConfigHelpers(t *testing.T) {
	if err := applyOpsRuntimeLogConfig(nil); err == nil {
		t.Fatalf("expected nil config error")
	}

	normalizeOpsRuntimeLogConfig(nil, &OpsRuntimeLogConfig{Level: "info"})
	normalizeOpsRuntimeLogConfig(&OpsRuntimeLogConfig{Level: "debug"}, nil)

	var nilSvc *OpsService
	nilSvc.applyRuntimeLogConfigOnStartup(context.Background())
}
