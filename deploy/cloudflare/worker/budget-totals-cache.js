import totalsSchema from "./budget-totals-schema.sql";
import totalsTriggers from "./budget-totals-triggers.sql";
import cacheVersion from "./budget-totals-version.sql";
import migrateCache from "./budget-totals-migrate.sql";
import setCacheVersion from "./budget-totals-set-version.sql";
import cacheState from "./budget-totals-state.sql";
import deleteState from "./budget-totals-delete-state.sql";
import completeCache from "./budget-totals-complete.sql";
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

const CACHE_VERSION = 3;

export function initializeUsageTotals(storage, width) {
  storage.transactionSync(function () {
    const previous = storage.sql.exec(cacheVersion).toArray()[0];
    if (previous?.version === CACHE_VERSION && previous.bucket_width_ms === width) {
      return;
    }
    storage.sql.exec(migrateCache);
    storage.sql.exec(totalsSchema);
    storage.sql.exec(totalsTriggers);
    storage.sql.exec(setCacheVersion, CACHE_VERSION, width);
  });
}

function discardBucket(storage, provider, model, width, start) {
  storage.sql.exec(deleteMarker, provider, model, width, start);
  storage.sql.exec(deleteGroups, provider, model, width, start);
}

export function backfillUsageTotals(storage, payload, width) {
  const { provider_id: provider, model } = payload;
  if (storage.sql.exec(cacheState, provider, model, width).toArray().length > 0) {
    return true;
  }
  storage.sql.exec(deleteState, provider, model, width);
  try {
    storage.sql.exec(resetMarkers, provider, model, width);
    storage.sql.exec(resetGroups, provider, model, width);
    const buckets = storage.sql.exec(occupiedBuckets, width, width, provider, model).toArray();
    for (const bucket of buckets) {
      storage.sql.exec(fillTotals, width, bucket.bucket_start_ms, provider, model,
        bucket.first_admitted_at_ms, bucket.last_admitted_at_ms);
      storage.sql.exec(markBucket, provider, model, width, bucket.bucket_start_ms,
        bucket.max_sequence, bucket.event_count, bucket.first_admitted_at_ms, bucket.last_admitted_at_ms);
    }
    storage.sql.exec(completeCache, provider, model, width);
    return true;
  } catch (error) {
    console.error(JSON.stringify({ message: "derived usage totals backfill failed", error: String(error) }));
    return false;
  }
}

// Begin before inserting raw usage, then finish with the returned cache state.
export function beginUsageTotalsUpdate(storage, provider, model, width) {
  const reusable = storage.sql.exec(cacheState, provider, model, width).toArray().length > 0;
  storage.sql.exec(deleteState, provider, model, width);
  return { provider, model, width, reusable };
}

export function finishUsageTotalsUpdate(storage, update, admittedAt, sequence) {
  const { provider, model, width, reusable } = update;
  if (!reusable) {
    return;
  }
  try {
    const start = Math.floor(admittedAt / width) * width;
    const previous = storage.sql.exec(reportState, provider, model, width, start).toArray()[0];
    storage.sql.exec(deleteMarker, provider, model, width, start);
    storage.sql.exec(addTotals, width, start, sequence);
    storage.sql.exec(markBucket, provider, model, width, start, sequence,
      (previous?.event_count ?? 0) + 1,
      Math.min(previous?.first_admitted_at_ms ?? admittedAt, admittedAt),
      Math.max(previous?.last_admitted_at_ms ?? admittedAt, admittedAt));
    storage.sql.exec(completeCache, provider, model, width);
  } catch (error) {
    console.error(JSON.stringify({ message: "derived usage totals update failed", sequence, error: String(error) }));
  }
}

export function invalidatePrunedUsageTotals(storage, sequence) {
  for (const bucket of storage.sql.exec(prunedBuckets, sequence).toArray()) {
    storage.sql.exec(deleteState, bucket.provider_id, bucket.model, bucket.bucket_width_ms);
    try {
      discardBucket(storage, bucket.provider_id, bucket.model, bucket.bucket_width_ms, bucket.bucket_start_ms);
    } catch (error) {
      console.error(JSON.stringify({ message: "derived usage totals invalidation failed", sequence, error: String(error) }));
    }
  }
}
