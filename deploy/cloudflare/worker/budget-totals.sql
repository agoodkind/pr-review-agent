WITH selected_usage AS (
  SELECT COALESCE(NULLIF(json_extract(sdk_usage, '$.model'), ''), model) AS reported_model,
    COALESCE(service_tier, '') AS service_tier,
    cached_input_tokens IS NOT NULL AS cached_known,
    cache_write_tokens IS NOT NULL AS writes_known,
    COALESCE(cache_write_tokens, 0) > 0 AS writes_positive,
    input_tokens = 0 OR (cached_input_tokens IS NOT NULL AND cache_write_tokens IS NOT NULL) AS pricing_ready,
    admitted_at_ms, input_tokens, output_tokens, total_tokens,
    cached_input_tokens, cache_write_tokens
  FROM usage_events LEFT JOIN usage_event_details USING (sequence)
  LEFT JOIN usage_event_sdk USING (sequence)
  WHERE provider_id = ? AND model = ? AND sequence <= ?
    AND (admitted_at_ms > ? OR (? = 1 AND admitted_at_ms = ?))
    AND (admitted_at_ms < ? OR (? = 1 AND admitted_at_ms = ?))
)
SELECT reported_model, service_tier, COUNT(*) AS requests,
  MIN(admitted_at_ms) AS first_admitted_at_ms,
  TOTAL(input_tokens) AS input_tokens, TOTAL(output_tokens) AS output_tokens,
  TOTAL(total_tokens) AS total_tokens,
  CASE WHEN cached_known THEN TOTAL(cached_input_tokens) ELSE NULL END AS cached_input_tokens,
  CASE WHEN writes_known THEN TOTAL(cache_write_tokens) ELSE NULL END AS cache_write_tokens
FROM selected_usage
GROUP BY reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready
ORDER BY reported_model COLLATE BINARY, service_tier COLLATE BINARY,
  cached_known, writes_known, writes_positive, pricing_ready
LIMIT ? OFFSET ?;
