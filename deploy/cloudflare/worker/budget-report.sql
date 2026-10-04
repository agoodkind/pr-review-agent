INSERT INTO usage_events
  (provider_id, model, admitted_at_ms, input_tokens, output_tokens, total_tokens)
VALUES (?, ?, ?, ?, ?, ?);
