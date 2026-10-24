-- Support batched conversation TPS lookups without scanning all usage history.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_user_session
    ON usage_logs (user_id, session_id)
    WHERE session_id IS NOT NULL;
