SELECT sequence, admitted_at_ms, input_tokens, output_tokens, total_tokens,
  cached_input_tokens, cache_write_tokens, service_tier
FROM usage_events LEFT JOIN usage_event_details USING (sequence)
WHERE provider_id = ? AND model = ? AND sequence > ? AND sequence <= ?
  AND (admitted_at_ms > ? OR (? = 1 AND admitted_at_ms = ?))
  AND (admitted_at_ms < ? OR (? = 1 AND admitted_at_ms = ?))
ORDER BY sequence
LIMIT ?;
