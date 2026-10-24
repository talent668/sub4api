package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/logger"
	"github.com/MACOS-DO/sub4api/internal/service"
	"go.uber.org/zap"
)

const usageSessionStatsTimeout = 2 * time.Second

type usageSessionKey struct {
	userID    int64
	sessionID string
}

// Optional metrics must not hide a successfully loaded usage list. Preserve
// caller cancellation, but degrade statistics failures to unavailable values.
func (r *usageLogRepository) hydrateUsageLogSessionStatsBestEffort(ctx context.Context, logs []service.UsageLog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.hydrateUsageLogSessionStats(ctx, logs); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		logger.L().Warn("usage session statistics unavailable", zap.Int("records", len(logs)), zap.Error(err))
	}
	return nil
}

// Batch only the sessions visible on this page, but aggregate their entire
// retained history. Pagination and display filters must not change a session's
// rate. User identity is part of the key to prevent cross-user correlation.
func (r *usageLogRepository) hydrateUsageLogSessionStats(ctx context.Context, logs []service.UsageLog) error {
	sessions := make(map[usageSessionKey]struct{})
	values := make([]string, 0, len(logs))
	args := make([]any, 0, len(logs)*2)
	for i := range logs {
		row := &logs[i]
		row.SessionAverageOutputTPS = nil
		row.SessionTPSRequestCount = 0
		if row.UserID <= 0 || row.SessionID == nil || strings.TrimSpace(*row.SessionID) == "" {
			continue
		}
		key := usageSessionKey{row.UserID, *row.SessionID}
		if _, exists := sessions[key]; exists {
			continue
		}
		sessions[key] = struct{}{}
		values = append(values, fmt.Sprintf("($%d::bigint, $%d::varchar)", len(args)+1, len(args)+2))
		args = append(args, key.userID, key.sessionID)
	}
	if len(values) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, usageSessionStatsTimeout)
	defer cancel()

	query := `SELECT ul.user_id, ul.session_id,
		SUM(ul.output_tokens)::double precision * 1000 / SUM(ul.duration_ms)::double precision,
		COUNT(*)
		FROM usage_logs ul
		JOIN (VALUES ` + strings.Join(values, ", ") + `) AS sessions(user_id, session_id)
			ON ul.user_id = sessions.user_id AND ul.session_id = sessions.session_id
		WHERE ul.session_id IS NOT NULL
			AND ul.output_tokens > 0 AND ul.duration_ms > 0
			AND ul.image_count = 0 AND ul.image_output_tokens = 0
			AND COALESCE(ul.billing_mode, '') <> 'image'
			AND ul.request_type IN (1, 2, 3, 4)
		GROUP BY ul.user_id, ul.session_id`
	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	type sessionStats struct {
		rate  float64
		count int64
	}
	stats := make(map[usageSessionKey]sessionStats, len(sessions))
	for rows.Next() {
		var key usageSessionKey
		var value sessionStats
		if err := rows.Scan(&key.userID, &key.sessionID, &value.rate, &value.count); err != nil {
			return err
		}
		stats[key] = value
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range logs {
		row := &logs[i]
		if row.SessionID == nil {
			continue
		}
		if value, ok := stats[usageSessionKey{row.UserID, *row.SessionID}]; ok {
			rate := value.rate
			row.SessionAverageOutputTPS = &rate
			row.SessionTPSRequestCount = value.count
		}
	}
	return nil
}
