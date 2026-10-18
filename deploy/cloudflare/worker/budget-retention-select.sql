SELECT sequence FROM usage_events
WHERE admitted_at_ms < ?
ORDER BY admitted_at_ms, sequence LIMIT ?;
