CREATE TABLE IF NOT EXISTS usage_history (
  provider_id TEXT PRIMARY KEY,
  history_start_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_events (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  provider_id TEXT NOT NULL,
  model TEXT NOT NULL,
  admitted_at_ms INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  total_tokens INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS usage_event_range
  ON usage_events (provider_id, model, admitted_at_ms, sequence);
