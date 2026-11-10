DELETE FROM usage_total_groups
WHERE provider_id = ? AND model = ? AND bucket_width_ms = ?;
