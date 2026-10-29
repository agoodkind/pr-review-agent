SELECT buckets.provider_id, buckets.model, buckets.bucket_width_ms, buckets.bucket_start_ms
FROM usage_events AS events JOIN usage_total_buckets AS buckets
  ON buckets.provider_id = events.provider_id AND buckets.model = events.model
    AND events.admitted_at_ms >= buckets.bucket_start_ms
    AND events.admitted_at_ms - buckets.bucket_start_ms < buckets.bucket_width_ms
WHERE events.sequence = ?;
