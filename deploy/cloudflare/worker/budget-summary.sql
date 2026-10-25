SELECT model AS requested_model,
  COALESCE(json_extract(sdk_usage, '$.model'), '') AS reported_model,
  COALESCE(service_tier, '') AS service_tier,
  COUNT(*) AS requests,
  SUM(input_tokens) AS input_tokens,
  SUM(output_tokens) AS output_tokens,
  SUM(total_tokens) AS total_tokens,
  SUM(COALESCE(cached_input_tokens, 0)) AS cached_input_tokens,
  SUM(COALESCE(cache_write_tokens, 0)) AS cache_write_tokens,
  (cached_input_tokens IS NOT NULL AND cache_write_tokens IS NOT NULL) AS cache_details_known
FROM usage_events LEFT JOIN usage_event_details USING (sequence)
LEFT JOIN usage_event_sdk USING (sequence)
WHERE provider_id = ? AND sequence <= ?
  AND admitted_at_ms >= ? AND admitted_at_ms <= ?
GROUP BY requested_model, reported_model, service_tier, cache_details_known,
  (COALESCE(cache_write_tokens, 0) > 0)
LIMIT ?;
