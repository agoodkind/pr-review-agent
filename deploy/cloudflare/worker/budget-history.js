import readHistory from "./budget-history-get.sql";
import createHistory from "./budget-history.sql";

export function usageHistoryStart(storage, provider, now) {
  const previous = storage.sql.exec(readHistory, provider).toArray()[0];
  if (previous !== undefined) {
    return previous.history_start_ms;
  }
  return storage.sql.exec(createHistory, provider, now).one().history_start_ms;
}
