import retentionSelect from "./budget-retention-select.sql";
import retentionSDK from "./budget-retention-sdk.sql";
import retentionDetails from "./budget-retention-details.sql";
import retentionEvent from "./budget-retention-event.sql";
import { invalidatePrunedUsageTotals } from "./budget-totals-cache.js";

export const dayMilliseconds = 86400000;

export async function sdkFingerprint(serialized) {
  if (serialized === "null") {
    return serialized;
  }
  const bytes = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(serialized));
  return "sha256:" + [...new Uint8Array(bytes)].map(function (byte) {
    return byte.toString(16).padStart(2, "0");
  }).join("");
}

export async function normalizeSDKFingerprint(value) {
  if (/^sha256:[a-f0-9]{64}$/.test(value)) {
    return value;
  }
  return sdkFingerprint(value);
}

export async function maintainUsageStorage(storage, settings, now) {
  return storage.transaction(async function (transaction) {
    const previous = (await transaction.get("usage_retention_cutoff_ms")) ?? 0;
    let cutoff = previous;
    if (settings.maximumAge > 0) {
      cutoff = Math.max(previous, now - settings.effectiveAge, 0);
      await transaction.put("usage_retention_cutoff_ms", cutoff);
      const rows = storage.sql.exec(retentionSelect, cutoff, settings.batchSize).toArray();
      if (rows.length > 0) {
        const generation = (await transaction.get("usage_totals_retention_generation")) ?? 0;
        if (!Number.isSafeInteger(generation + 1)) {
          throw new Error("usage totals retention generation exceeds the safe integer range");
        }
        await transaction.put("usage_totals_retention_generation", generation + 1);
      }
      for (const row of rows) {
        invalidatePrunedUsageTotals(storage, row.sequence);
        storage.sql.exec(retentionSDK, row.sequence);
        storage.sql.exec(retentionDetails, row.sequence);
        storage.sql.exec(retentionEvent, row.sequence);
      }
    }
    const moreIdentities = await maintainIdentities(transaction, settings, cutoff);
    if (!moreIdentities) {
      await transaction.put("usage_sdk_digest_migration_version", 1);
    }
    if (settings.maximumAge > 0) {
      for (const prefix of ["reported_tokens:", "tokens:"]) {
        await maintainDailyCounters(transaction, settings, prefix, cutoff, now);
      }
    }
    return settings.maximumAge > 0 || moreIdentities;
  });
}

async function scanBatch(transaction, settings, prefix) {
  const cursorKey = `usage_retention_cursor:${prefix}`;
  const cursor = await transaction.get(cursorKey);
  const options = { prefix, limit: settings.batchSize };
  if (cursor !== undefined) {
    options.startAfter = cursor;
  }
  const entries = await transaction.list(options);
  if (entries.size === settings.batchSize) {
    await transaction.put(cursorKey, [...entries.keys()].at(-1));
  } else {
    await transaction.delete(cursorKey);
  }
  return entries;
}

async function maintainIdentities(transaction, settings, cutoff) {
  const entries = await scanBatch(transaction, settings, "usage_report:");
  for (const [key, value] of entries) {
    const admittedAt = JSON.parse(value)[2];
    const identity = key.slice("usage_report:".length);
    const sdkKey = `usage_report_sdk:${identity}`;
    if (settings.maximumAge > 0 && admittedAt < cutoff) {
      await transaction.delete([key, `usage_report_details:${identity}`, sdkKey]);
      continue;
    }
    const previous = await transaction.get(sdkKey);
    if (previous !== undefined) {
      const digest = await normalizeSDKFingerprint(previous);
      if (previous !== digest) {
        await transaction.put(sdkKey, digest);
      }
    }
  }
  return entries.size === settings.batchSize;
}

async function maintainDailyCounters(transaction, settings, prefix, cutoff, now) {
  const entries = await scanBatch(transaction, settings, prefix);
  const currentDay = new Date(now).toISOString().slice(0, 10);
  for (const key of entries.keys()) {
    const day = key.slice(prefix.length, prefix.length + 10);
    const end = Date.parse(day + "T00:00:00Z") + dayMilliseconds;
    if (day < currentDay && end <= cutoff) {
      await transaction.delete(key);
    }
  }
}
