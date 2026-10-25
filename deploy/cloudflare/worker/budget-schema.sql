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
CREATE INDEX IF NOT EXISTS usage_event_retention
  ON usage_events (admitted_at_ms, sequence);
CREATE INDEX IF NOT EXISTS usage_provider_period
  ON usage_events (provider_id, admitted_at_ms, sequence);
CREATE TABLE IF NOT EXISTS usage_event_details (
  sequence INTEGER PRIMARY KEY,
  cached_input_tokens INTEGER,
  cache_write_tokens INTEGER,
  service_tier TEXT
);
CREATE TABLE IF NOT EXISTS usage_event_sdk (
  sequence INTEGER PRIMARY KEY,
  sdk_usage TEXT NOT NULL
);
