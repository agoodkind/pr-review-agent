WITH settings AS (
  SELECT ? AS provider_id, ? AS model, ? AS width, ? AS snapshot,
    ? AS start_ms, ? AS include_start, ? AS end_ms, ? AS include_end,
    ? AS page_size, ? AS after_group
), occupied AS (
  SELECT CAST(events.admitted_at_ms / settings.width AS INTEGER) * settings.width AS bucket_start_ms,
    COUNT(*) AS event_count, MAX(events.sequence) AS max_sequence
  FROM usage_events AS events CROSS JOIN settings
  WHERE events.provider_id = settings.provider_id AND events.model = settings.model
    AND (events.admitted_at_ms > settings.start_ms OR (settings.include_start = 1 AND events.admitted_at_ms = settings.start_ms))
    AND (events.admitted_at_ms < settings.end_ms OR (settings.include_end = 1 AND events.admitted_at_ms = settings.end_ms))
  GROUP BY bucket_start_ms
), eligible AS (
  SELECT buckets.bucket_start_ms
  FROM usage_total_buckets AS buckets JOIN occupied USING (bucket_start_ms) CROSS JOIN settings
  WHERE buckets.provider_id = settings.provider_id AND buckets.model = settings.model
    AND buckets.bucket_width_ms = settings.width AND buckets.max_sequence <= settings.snapshot
    AND buckets.event_count = occupied.event_count AND buckets.max_sequence = occupied.max_sequence
    AND (buckets.bucket_start_ms > settings.start_ms OR (settings.include_start = 1 AND buckets.bucket_start_ms = settings.start_ms))
    AND (buckets.bucket_start_ms + settings.width - 1 < settings.end_ms OR
      (settings.include_end = 1 AND buckets.bucket_start_ms + settings.width - 1 = settings.end_ms))
), raw_groups AS (
  SELECT reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready,
    COUNT(*) AS requests, MIN(admitted_at_ms) AS first_admitted_at_ms,
    TOTAL(input_tokens) AS input_tokens, TOTAL(output_tokens) AS output_tokens, TOTAL(total_tokens) AS total_tokens,
    CASE WHEN cached_known THEN TOTAL(cached_input_tokens) ELSE NULL END AS cached_input_tokens,
    CASE WHEN writes_known THEN TOTAL(cache_write_tokens) ELSE NULL END AS cache_write_tokens
  FROM usage_total_inputs CROSS JOIN settings
  WHERE usage_total_inputs.provider_id = settings.provider_id AND usage_total_inputs.model = settings.model
    AND sequence <= settings.snapshot
    AND (admitted_at_ms > settings.start_ms OR (settings.include_start = 1 AND admitted_at_ms = settings.start_ms))
    AND (admitted_at_ms < settings.end_ms OR (settings.include_end = 1 AND admitted_at_ms = settings.end_ms))
    AND NOT EXISTS (SELECT 1 FROM eligible WHERE eligible.bucket_start_ms = CAST(admitted_at_ms / settings.width AS INTEGER) * settings.width)
  GROUP BY reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready
), combined AS (
  SELECT * FROM raw_groups
  UNION ALL
  SELECT reported_model, service_tier, cached_known, writes_known, writes_positive, pricing_ready,
    requests, first_admitted_at_ms, input_tokens, output_tokens, total_tokens, cached_input_tokens, cache_write_tokens
  FROM usage_total_groups AS groups JOIN eligible USING (bucket_start_ms) CROSS JOIN settings
  WHERE groups.provider_id = settings.provider_id AND groups.model = settings.model AND groups.bucket_width_ms = settings.width
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
LIMIT (SELECT page_size FROM settings) OFFSET (SELECT after_group FROM settings);
