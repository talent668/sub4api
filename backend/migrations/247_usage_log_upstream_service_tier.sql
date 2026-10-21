-- Keep the service tier echoed by the upstream response separate from the
-- outbound request tier, so operators can tell whether the upstream honored
-- the requested tier (e.g. outbound priority but upstream echo default).
ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS upstream_service_tier VARCHAR(16);

COMMENT ON COLUMN usage_logs.upstream_service_tier IS
    'Service tier echoed by the upstream response; NULL when the response declared none';
