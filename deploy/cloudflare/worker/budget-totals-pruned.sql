SELECT events.provider_id, events.model, version.bucket_width_ms,
  CAST(events.admitted_at_ms / version.bucket_width_ms AS INTEGER) * version.bucket_width_ms AS bucket_start_ms
FROM usage_events AS events CROSS JOIN usage_total_cache_version AS version
WHERE events.sequence = ? AND version.id = 1;
