WITH eligible AS (
  SELECT bucket_start_ms FROM usage_total_buckets
  WHERE provider_id = ?1 AND model = ?2 AND bucket_width_ms = ?3
    AND max_sequence <= ?4
    AND first_admitted_at_ms >= ?5 AND last_admitted_at_ms <= ?6
    AND ?7 = 1
), raw_ranges AS (
  SELECT MAX(first_admitted_at_ms, ?5) AS start_ms,
    MIN(last_admitted_at_ms, ?6) AS end_ms
  FROM usage_total_buckets
  WHERE provider_id = ?1 AND model = ?2 AND bucket_width_ms = ?3
    AND last_admitted_at_ms >= ?5 AND first_admitted_at_ms <= ?6
    AND (first_admitted_at_ms < ?5 OR last_admitted_at_ms > ?6 OR max_sequence > ?4)
    AND ?7 = 1
  UNION ALL
  SELECT ?5, ?6 WHERE ?7 = 0 AND ?5 <= ?6
), raw_groups AS (
  SELECT reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready,
    COUNT(*) AS requests, MIN(admitted_at_ms) AS first_admitted_at_ms,
    TOTAL(input_tokens) AS input_tokens, TOTAL(output_tokens) AS output_tokens, TOTAL(total_tokens) AS total_tokens,
    CASE WHEN cached_known THEN TOTAL(cached_input_tokens) ELSE NULL END AS cached_input_tokens,
    CASE WHEN writes_known THEN TOTAL(cache_write_tokens) ELSE NULL END AS cache_write_tokens
  FROM raw_ranges CROSS JOIN usage_total_inputs
  WHERE provider_id = ?1 AND model = ?2 AND sequence <= ?4
    AND admitted_at_ms >= raw_ranges.start_ms AND admitted_at_ms <= raw_ranges.end_ms
  GROUP BY reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready
), combined AS (
  SELECT * FROM raw_groups
  UNION ALL
  SELECT reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready,
    requests, first_admitted_at_ms, input_tokens, output_tokens, total_tokens, cached_input_tokens, cache_write_tokens
  FROM eligible CROSS JOIN usage_total_groups AS groups
  WHERE groups.provider_id = ?1 AND groups.model = ?2 AND groups.bucket_width_ms = ?3
    AND groups.bucket_start_ms = eligible.bucket_start_ms
)
SELECT reported_model, service_tier, TOTAL(requests) AS requests,
  MIN(first_admitted_at_ms) AS first_admitted_at_ms,
  TOTAL(input_tokens) AS input_tokens, TOTAL(output_tokens) AS output_tokens, TOTAL(total_tokens) AS total_tokens,
  CASE WHEN cached_known THEN TOTAL(cached_input_tokens) ELSE NULL END AS cached_input_tokens,
  CASE WHEN writes_known THEN TOTAL(cache_write_tokens) ELSE NULL END AS cache_write_tokens
FROM combined
GROUP BY reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready
ORDER BY reported_model COLLATE BINARY, service_tier COLLATE BINARY,
  cached_known, writes_known, writes_positive, pricing_ready
LIMIT ?8 OFFSET ?9;
