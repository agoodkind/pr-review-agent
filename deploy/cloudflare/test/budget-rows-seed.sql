WITH RECURSIVE numbers(value) AS (
  VALUES(1)
  UNION ALL
  SELECT value + 1 FROM numbers WHERE value < ?
)
INSERT INTO usage_events (
  provider_id, model, admitted_at_ms, input_tokens, output_tokens, total_tokens
)
SELECT ?, ?, ? + value, 11, 7, 20 FROM numbers;
