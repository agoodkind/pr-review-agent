WITH occupied AS (
  SELECT CAST(admitted_at_ms / ? AS INTEGER) * ? AS bucket_start_ms,
    COUNT(*) AS event_count, MAX(sequence) AS max_sequence
  FROM usage_events
  WHERE provider_id = ? AND model = ?
    AND (admitted_at_ms > ? OR (? = 1 AND admitted_at_ms = ?))
    AND (admitted_at_ms < ? OR (? = 1 AND admitted_at_ms = ?))
  GROUP BY bucket_start_ms
)
SELECT occupied.*, buckets.event_count AS cached_count, buckets.max_sequence AS cached_sequence
FROM occupied LEFT JOIN usage_total_buckets AS buckets
  ON buckets.provider_id = ? AND buckets.model = ? AND buckets.bucket_width_ms = ?
    AND buckets.bucket_start_ms = occupied.bucket_start_ms
ORDER BY occupied.bucket_start_ms;
