WITH settings AS (
  SELECT ? AS provider_id, ? AS model, ? AS bucket_width_ms,
    ? AS bucket_start_ms, ? AS bucket_last_ms, ? AS sequence
)
SELECT buckets.event_count, buckets.max_sequence,
  (SELECT COUNT(*) FROM usage_events AS events
   WHERE events.provider_id = settings.provider_id AND events.model = settings.model
     AND events.admitted_at_ms >= settings.bucket_start_ms AND events.admitted_at_ms <= settings.bucket_last_ms
     AND events.sequence < settings.sequence) AS previous_count,
  (SELECT COALESCE(MAX(events.sequence), 0) FROM usage_events AS events
   WHERE events.provider_id = settings.provider_id AND events.model = settings.model
     AND events.admitted_at_ms >= settings.bucket_start_ms AND events.admitted_at_ms <= settings.bucket_last_ms
     AND events.sequence < settings.sequence) AS previous_sequence
FROM usage_total_buckets AS buckets CROSS JOIN settings
WHERE buckets.provider_id = settings.provider_id AND buckets.model = settings.model
  AND buckets.bucket_width_ms = settings.bucket_width_ms AND buckets.bucket_start_ms = settings.bucket_start_ms;
