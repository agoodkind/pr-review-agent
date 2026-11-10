INSERT INTO usage_total_groups (
  provider_id, model, bucket_width_ms, bucket_start_ms,
  reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready,
  requests, first_admitted_at_ms, input_tokens, output_tokens, total_tokens,
  cached_input_tokens, cache_write_tokens
)
SELECT provider_id, model, ?, ?,
  reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready,
  COUNT(*), MIN(admitted_at_ms), TOTAL(input_tokens), TOTAL(output_tokens), TOTAL(total_tokens),
  CASE WHEN cached_known THEN TOTAL(cached_input_tokens) ELSE NULL END,
  CASE WHEN writes_known THEN TOTAL(cache_write_tokens) ELSE NULL END
FROM usage_total_inputs
WHERE provider_id = ? AND model = ? AND admitted_at_ms >= ? AND admitted_at_ms <= ?
GROUP BY reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready;
