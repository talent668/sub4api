package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	defaultRequestDiagnosticRetentionDays = 7
	requestDiagnosticRetentionInterval    = time.Minute
	requestDiagnosticRetentionTimeout     = 15 * time.Second
	requestDiagnosticRetentionBackoff     = 5 * time.Second
	requestDiagnosticRetentionBackoffMax  = 5 * time.Minute
)

// RequestDiagnosticRetentionPolicy applies only to allowlisted request diagnostic
// rows. MaxSizeMB is a logical storage quota, not physical PostgreSQL disk usage;
// zero disables the size limit. Policies are copied before atomic publication.
type RequestDiagnosticRetentionPolicy struct {
	RetentionDays int
	MaxSizeMB     int
}

// RequestDiagnosticRetentionResult describes one bounded repository pass. An
// incomplete scan is deliberately retried at the normal cadence, not in a loop.
type RequestDiagnosticRetentionResult struct {
	DeletedExpired int64 `json:"deleted_expired"`
	DeletedForSize int64 `json:"deleted_for_size"`
	ScannedRows    int64 `json:"scanned_rows"`
	LockSkipped    bool  `json:"lock_skipped"`
	ScanIncomplete bool  `json:"scan_incomplete"`
	HasMore        bool  `json:"has_more"`
}

// RequestDiagnosticRetentionHealth exposes bounded passes that could not finish,
// instead of reporting a silently unenforced quota as successful maintenance.
type RequestDiagnosticRetentionHealth struct {
	PolicyConfigured bool                             `json:"policy_configured"`
	LastRunAt        string                           `json:"last_run_at"`
	LastError        string                           `json:"last_error"`
	Result           RequestDiagnosticRetentionResult `json:"result"`
}

func (s *OpsSystemLogSink) requestDiagnosticRetentionHealth() RequestDiagnosticRetentionHealth {
	var health RequestDiagnosticRetentionHealth
	if last := s.diagnosticRetentionLastRun.Load(); last != nil {
		health = *last
	}
	health.PolicyConfigured = s.diagnosticRetentionPolicy.Load() != nil
	return health
}

// RequestDiagnosticLogMaintainer is optional so unrelated Ops repositories and
// mocks need not implement retention. All database work belongs to the repository.
type RequestDiagnosticLogMaintainer interface {
	MaintainRequestDiagnosticLogs(context.Context, RequestDiagnosticRetentionPolicy) (RequestDiagnosticRetentionResult, error)
}

func validateRequestDiagnosticRetentionPolicy(policy RequestDiagnosticRetentionPolicy) error {
	if policy.RetentionDays < 1 || policy.RetentionDays > 3650 {
		return errors.New("diagnostic_retention_days must be between 1 and 3650")
	}
	if policy.MaxSizeMB < 0 || policy.MaxSizeMB > 102400 {
		return errors.New("diagnostic_max_size_mb must be between 0 and 102400")
	}
	return nil
}

// SetRequestDiagnosticRetentionPolicy must only receive successfully persisted
// administrative changes or a valid loaded/default policy. It never performs I/O
// and does not couple historical cleanup to diagnostic capture or Ops monitoring.
func (s *OpsSystemLogSink) SetRequestDiagnosticRetentionPolicy(policy RequestDiagnosticRetentionPolicy) error {
	if err := validateRequestDiagnosticRetentionPolicy(policy); err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	previous := s.diagnosticRetentionPolicy.Swap(&policy)
	if previous == nil || *previous != policy {
		s.wakeRequestDiagnosticRetention()
	}
	return nil
}

func (s *OpsSystemLogSink) wakeRequestDiagnosticRetention() {
	if s == nil || s.diagnosticRetentionWake == nil {
		return
	}
	select {
	case s.diagnosticRetentionWake <- struct{}{}:
	default:
	}
}

func (s *OpsSystemLogSink) runRequestDiagnosticRetention(maintainer RequestDiagnosticLogMaintainer) {
	defer s.wg.Done()
	interval := s.diagnosticRetentionInterval
	if interval <= 0 {
		interval = requestDiagnosticRetentionInterval
	}
	timeout := s.diagnosticRetentionTimeout
	if timeout <= 0 {
		timeout = requestDiagnosticRetentionTimeout
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	failures := 0
	var nextAttempt, lastAttempt time.Time
	var lastPolicy RequestDiagnosticRetentionPolicy
	attempt := func(wake bool) {
		if s.ctx.Err() != nil || time.Now().Before(nextAttempt) {
			return
		}
		// A nil pointer means policyConfigured=false. In particular, construction
		// alone must never authorize deletion while settings storage is unavailable.
		policy := s.diagnosticRetentionPolicy.Load()
		if policy == nil {
			return
		}
		if wake && *policy == lastPolicy && time.Since(lastAttempt) < interval {
			return
		}
		lastAttempt = time.Now()
		lastPolicy = *policy
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		result, err := maintainer.MaintainRequestDiagnosticLogs(ctx, *policy)
		cancel()
		if s.ctx.Err() != nil {
			return
		}
		health := &RequestDiagnosticRetentionHealth{
			LastRunAt: time.Now().UTC().Format(time.RFC3339Nano),
			Result:    result,
		}
		if err != nil {
			health.LastError = err.Error()
		}
		s.diagnosticRetentionLastRun.Store(health)
		if err != nil {
			failures++
			backoff := s.requestDiagnosticRetentionBackoffFor(failures)
			nextAttempt = time.Now().Add(backoff)
			// Do not send worker errors through the sink it is maintaining.
			_, _ = fmt.Fprintf(os.Stderr, "time=%s level=WARN msg=\"request diagnostic retention failed\" err=%v failures=%d backoff=%s\n",
				time.Now().Format(time.RFC3339Nano), err, failures, backoff)
			return
		}
		failures = 0
		nextAttempt = time.Time{}
	}

	attempt(false) // Handles a policy published before Start, even without a wake.
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			attempt(false)
		case <-s.diagnosticRetentionWake:
			attempt(true)
		}
	}
}

func (s *OpsSystemLogSink) requestDiagnosticRetentionBackoffFor(failures int) time.Duration {
	base := s.diagnosticRetentionBackoff
	if base <= 0 {
		base = requestDiagnosticRetentionBackoff
	}
	maxBackoff := s.diagnosticRetentionBackoffMax
	if maxBackoff <= 0 {
		maxBackoff = requestDiagnosticRetentionBackoffMax
	}
	if maxBackoff < base {
		maxBackoff = base
	}
	backoff := base
	for i := 1; i < failures && backoff < maxBackoff; i++ {
		if backoff > maxBackoff/2 {
			return maxBackoff
		}
		backoff *= 2
	}
	return backoff
}
