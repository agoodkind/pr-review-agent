SELECT CAST(admitted_at_ms / ? AS INTEGER) * ? AS bucket_start_ms,
  COUNT(*) AS event_count, MAX(sequence) AS max_sequence,
  MIN(admitted_at_ms) AS first_admitted_at_ms,
  MAX(admitted_at_ms) AS last_admitted_at_ms
FROM usage_events
WHERE provider_id = ? AND model = ?
GROUP BY bucket_start_ms
ORDER BY bucket_start_ms;
