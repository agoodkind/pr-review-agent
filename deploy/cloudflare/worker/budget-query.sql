SELECT sequence, admitted_at_ms, input_tokens, output_tokens, total_tokens
FROM usage_events
WHERE provider_id = ? AND model = ? AND sequence > ? AND sequence <= ?
  AND (admitted_at_ms > ? OR (? = 1 AND admitted_at_ms = ?))
  AND (admitted_at_ms < ? OR (? = 1 AND admitted_at_ms = ?))
ORDER BY sequence
LIMIT ?;
