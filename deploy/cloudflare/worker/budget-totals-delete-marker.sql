DELETE FROM usage_total_buckets
WHERE provider_id = ? AND model = ? AND bucket_width_ms = ? AND bucket_start_ms = ?;
