INSERT INTO usage_total_groups (
  provider_id, model, bucket_width_ms, bucket_start_ms,
  reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready,
  requests, first_admitted_at_ms, input_tokens, output_tokens, total_tokens,
  cached_input_tokens, cache_write_tokens
)
SELECT provider_id, model, ?, ?,
  reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready,
  1, admitted_at_ms, input_tokens, output_tokens, total_tokens, cached_input_tokens, cache_write_tokens
FROM usage_total_inputs WHERE sequence = ?
ON CONFLICT (provider_id, model, bucket_width_ms, bucket_start_ms,
  reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready)
DO UPDATE SET requests = requests + excluded.requests,
  first_admitted_at_ms = MIN(first_admitted_at_ms, excluded.first_admitted_at_ms),
  input_tokens = input_tokens + excluded.input_tokens,
  output_tokens = output_tokens + excluded.output_tokens,
  total_tokens = total_tokens + excluded.total_tokens,
  cached_input_tokens = cached_input_tokens + excluded.cached_input_tokens,
  cache_write_tokens = cache_write_tokens + excluded.cache_write_tokens;
