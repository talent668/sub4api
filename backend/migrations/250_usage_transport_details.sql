-- Nullable, append-only observation metadata. Historical routes stay unknown.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS transport_details JSONB;
