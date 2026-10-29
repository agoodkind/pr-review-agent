import occupiedBuckets from "./budget-totals-occupied.sql";
import fillTotals from "./budget-totals-fill.sql";
import markBucket from "./budget-totals-mark.sql";
import deleteMarker from "./budget-totals-delete-marker.sql";
import deleteGroups from "./budget-totals-delete-groups.sql";
import resetMarkers from "./budget-totals-reset.sql";
import resetGroups from "./budget-totals-reset-groups.sql";
import reportState from "./budget-totals-report-state.sql";
import addTotals from "./budget-totals-add.sql";
import prunedBuckets from "./budget-totals-pruned.sql";

function discardBucket(storage, provider, model, width, start) {
  storage.sql.exec(deleteMarker, provider, model, width, start);
  storage.sql.exec(deleteGroups, provider, model, width, start);
}

function containsBucket(payload, start, width) {
  const last = start + width - 1;
  return Number.isSafeInteger(last) &&
    (start > payload.start_ms || (payload.include_start && start === payload.start_ms)) &&
    (last < payload.end_ms || (payload.include_end && last === payload.end_ms));
}

export function backfillUsageTotals(storage, payload, width) {
  try {
    storage.sql.exec(resetMarkers, width);
    storage.sql.exec(resetGroups, width);
    const { provider_id: provider, model, start_ms: start, end_ms: end,
      include_start: includeStart, include_end: includeEnd } = payload;
    const buckets = storage.sql.exec(occupiedBuckets, width, width, provider, model,
      start, Number(includeStart), start, end, Number(includeEnd), end, provider, model, width).toArray();
    for (const bucket of buckets) {
      if (!containsBucket(payload, bucket.bucket_start_ms, width) ||
          (bucket.event_count === bucket.cached_count && bucket.max_sequence === bucket.cached_sequence)) {
        continue;
      }
      discardBucket(storage, provider, model, width, bucket.bucket_start_ms);
      storage.sql.exec(fillTotals, width, bucket.bucket_start_ms, provider, model,
        bucket.bucket_start_ms, bucket.bucket_start_ms + width - 1);
      storage.sql.exec(markBucket, provider, model, width, bucket.bucket_start_ms,
        bucket.max_sequence, bucket.event_count);
    }
  } catch (error) {
    console.error(JSON.stringify({ message: "derived usage totals backfill failed", error: String(error) }));
  }
}

export function updateUsageTotals(storage, provider, model, admittedAt, sequence, width) {
  try {
    const start = Math.floor(admittedAt / width) * width;
    const last = start + width - 1;
    if (!Number.isSafeInteger(last)) {
      return;
    }
    const rows = storage.sql.exec(reportState, provider, model, width, start, last, sequence).toArray();
    if (rows.length === 0) {
      return;
    }
    const previous = rows[0];
    storage.sql.exec(deleteMarker, provider, model, width, start);
    if (previous.event_count !== previous.previous_count || previous.max_sequence !== previous.previous_sequence) {
      storage.sql.exec(deleteGroups, provider, model, width, start);
      return;
    }
    storage.sql.exec(addTotals, width, start, sequence);
    storage.sql.exec(markBucket, provider, model, width, start, sequence, previous.event_count + 1);
  } catch (error) {
    console.error(JSON.stringify({ message: "derived usage totals update failed", sequence, error: String(error) }));
  }
}

export function invalidatePrunedUsageTotals(storage, sequence) {
  try {
    for (const bucket of storage.sql.exec(prunedBuckets, sequence).toArray()) {
      discardBucket(storage, bucket.provider_id, bucket.model, bucket.bucket_width_ms, bucket.bucket_start_ms);
    }
  } catch (error) {
    console.error(JSON.stringify({ message: "derived usage totals invalidation failed", sequence, error: String(error) }));
  }
}
