CREATE TRIGGER usage_total_cache_event_insert AFTER INSERT ON usage_events
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE provider_id = NEW.provider_id AND model = NEW.model;
END;
CREATE TRIGGER usage_total_cache_event_update AFTER UPDATE ON usage_events
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE provider_id = OLD.provider_id AND model = OLD.model;
  DELETE FROM usage_total_cache_state
  WHERE provider_id = NEW.provider_id AND model = NEW.model;
END;
CREATE TRIGGER usage_total_cache_event_delete AFTER DELETE ON usage_events
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE provider_id = OLD.provider_id AND model = OLD.model;
END;
CREATE TRIGGER usage_total_cache_details_insert AFTER INSERT ON usage_event_details
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE (provider_id, model) = (
    SELECT provider_id, model FROM usage_events WHERE sequence = NEW.sequence
  );
END;
CREATE TRIGGER usage_total_cache_details_delete AFTER DELETE ON usage_event_details
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE (provider_id, model) = (
    SELECT provider_id, model FROM usage_events WHERE sequence = OLD.sequence
  );
END;
CREATE TRIGGER usage_total_cache_sdk_insert AFTER INSERT ON usage_event_sdk
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE (provider_id, model) = (
    SELECT provider_id, model FROM usage_events WHERE sequence = NEW.sequence
  );
END;
CREATE TRIGGER usage_total_cache_sdk_delete AFTER DELETE ON usage_event_sdk
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE (provider_id, model) = (
    SELECT provider_id, model FROM usage_events WHERE sequence = OLD.sequence
  );
END;
CREATE TRIGGER usage_total_cache_bucket_insert AFTER INSERT ON usage_total_buckets
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE provider_id = NEW.provider_id AND model = NEW.model AND bucket_width_ms = NEW.bucket_width_ms;
END;
CREATE TRIGGER usage_total_cache_bucket_update AFTER UPDATE ON usage_total_buckets
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE provider_id = OLD.provider_id AND model = OLD.model AND bucket_width_ms = OLD.bucket_width_ms;
  DELETE FROM usage_total_cache_state
  WHERE provider_id = NEW.provider_id AND model = NEW.model AND bucket_width_ms = NEW.bucket_width_ms;
END;
CREATE TRIGGER usage_total_cache_bucket_delete AFTER DELETE ON usage_total_buckets
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE provider_id = OLD.provider_id AND model = OLD.model AND bucket_width_ms = OLD.bucket_width_ms;
END;
CREATE TRIGGER usage_total_cache_group_insert AFTER INSERT ON usage_total_groups
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE provider_id = NEW.provider_id AND model = NEW.model AND bucket_width_ms = NEW.bucket_width_ms;
END;
CREATE TRIGGER usage_total_cache_group_update AFTER UPDATE ON usage_total_groups
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE provider_id = OLD.provider_id AND model = OLD.model AND bucket_width_ms = OLD.bucket_width_ms;
  DELETE FROM usage_total_cache_state
  WHERE provider_id = NEW.provider_id AND model = NEW.model AND bucket_width_ms = NEW.bucket_width_ms;
END;
CREATE TRIGGER usage_total_cache_group_delete AFTER DELETE ON usage_total_groups
BEGIN
  DELETE FROM usage_total_cache_state
  WHERE provider_id = OLD.provider_id AND model = OLD.model AND bucket_width_ms = OLD.bucket_width_ms;
END;
