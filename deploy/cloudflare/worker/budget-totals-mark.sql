INSERT OR REPLACE INTO usage_total_buckets
  (provider_id, model, bucket_width_ms, bucket_start_ms, max_sequence, event_count)
VALUES (?, ?, ?, ?, ?, ?);
