DELETE FROM usage_total_cache_state
WHERE provider_id = ? AND model = ? AND bucket_width_ms = ?;
