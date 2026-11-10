CREATE TABLE IF NOT EXISTS usage_total_cache_version (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  version INTEGER NOT NULL,
  bucket_width_ms INTEGER NOT NULL
);
SELECT version, bucket_width_ms FROM usage_total_cache_version WHERE id = 1;
