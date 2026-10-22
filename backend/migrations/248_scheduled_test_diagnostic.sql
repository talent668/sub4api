-- 248_scheduled_test_diagnostic.sql
-- Extend scheduled tests with Codex degradation-diagnostic plans.

ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS test_type VARCHAR(32) NOT NULL DEFAULT 'account_test',
    ADD COLUMN IF NOT EXISTS api_key_id BIGINT REFERENCES api_keys(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS models JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE scheduled_test_results
    ADD COLUMN IF NOT EXISTS model_id VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS predicted_model VARCHAR(100) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS probability DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS reason VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS http_status INT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_stp_test_type ON scheduled_test_plans(test_type);

-- Existing plans are account connectivity tests with a single model.
UPDATE scheduled_test_plans
SET test_type = 'account_test'
WHERE test_type = '' OR test_type IS NULL;
