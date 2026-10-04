INSERT INTO usage_history (provider_id, history_start_ms) VALUES (?, ?)
ON CONFLICT(provider_id) DO UPDATE SET provider_id = excluded.provider_id
RETURNING history_start_ms;
