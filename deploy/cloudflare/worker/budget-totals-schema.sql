CREATE TABLE IF NOT EXISTS usage_total_buckets (
  provider_id TEXT NOT NULL,
  model TEXT NOT NULL,
  bucket_width_ms INTEGER NOT NULL,
  bucket_start_ms INTEGER NOT NULL,
  max_sequence INTEGER NOT NULL,
  event_count INTEGER NOT NULL,
  PRIMARY KEY (provider_id, model, bucket_width_ms, bucket_start_ms)
);
CREATE TABLE IF NOT EXISTS usage_total_groups (
  provider_id TEXT NOT NULL,
  model TEXT NOT NULL,
  bucket_width_ms INTEGER NOT NULL,
  bucket_start_ms INTEGER NOT NULL,
  reported_model TEXT NOT NULL,
  service_tier TEXT NOT NULL,
  cached_known INTEGER NOT NULL,
  writes_known INTEGER NOT NULL,
  writes_positive INTEGER NOT NULL,
  pricing_ready INTEGER NOT NULL,
  requests INTEGER NOT NULL CHECK (requests > 0 AND requests <= 9007199254740991),
  first_admitted_at_ms INTEGER NOT NULL CHECK (first_admitted_at_ms >= 0 AND first_admitted_at_ms <= 9007199254740991),
  input_tokens INTEGER NOT NULL CHECK (input_tokens >= 0 AND input_tokens <= 9007199254740991),
  output_tokens INTEGER NOT NULL CHECK (output_tokens >= 0 AND output_tokens <= 9007199254740991),
  total_tokens INTEGER NOT NULL CHECK (total_tokens >= 0 AND total_tokens <= 9007199254740991),
  cached_input_tokens INTEGER CHECK (cached_input_tokens IS NULL OR (cached_input_tokens >= 0 AND cached_input_tokens <= 9007199254740991)),
  cache_write_tokens INTEGER CHECK (cache_write_tokens IS NULL OR (cache_write_tokens >= 0 AND cache_write_tokens <= 9007199254740991)),
  PRIMARY KEY (provider_id, model, bucket_width_ms, bucket_start_ms,
    reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready)
);
CREATE VIEW IF NOT EXISTS usage_total_inputs AS
  SELECT provider_id, model, sequence, admitted_at_ms,
    COALESCE(NULLIF(json_extract(sdk_usage, '$.model'), ''), model) AS reported_model,
    COALESCE(service_tier, '') AS service_tier,
    cached_input_tokens IS NOT NULL AS cached_known,
    cache_write_tokens IS NOT NULL AS writes_known,
    COALESCE(cache_write_tokens, 0) > 0 AS writes_positive,
    input_tokens = 0 OR (cached_input_tokens IS NOT NULL AND cache_write_tokens IS NOT NULL) AS pricing_ready,
    input_tokens, output_tokens, total_tokens, cached_input_tokens, cache_write_tokens
  FROM usage_events LEFT JOIN usage_event_details USING (sequence)
  LEFT JOIN usage_event_sdk USING (sequence);
