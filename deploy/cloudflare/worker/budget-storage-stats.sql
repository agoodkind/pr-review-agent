SELECT
  (SELECT COUNT(*) FROM usage_events) AS events,
  (SELECT COUNT(*) FROM usage_event_details) AS details,
  (SELECT COUNT(*) FROM usage_event_sdk) AS sdk_records,
  (SELECT COALESCE(SUM(length(CAST(sdk_usage AS BLOB))), 0) FROM usage_event_sdk) AS sdk_payload_bytes,
  (SELECT MIN(admitted_at_ms) FROM usage_events) AS oldest_admission_ms,
  (SELECT MAX(admitted_at_ms) FROM usage_events) AS newest_admission_ms;
