SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'usage_events'), 0) AS sequence;
