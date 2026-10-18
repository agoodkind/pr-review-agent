import runtime from "../../../runtime.json" with { type: "json" };
import retentionSelect from "./budget-retention-select.sql";
import retentionSDK from "./budget-retention-sdk.sql";
import retentionDetails from "./budget-retention-details.sql";
import retentionEvent from "./budget-retention-event.sql";

const durationUnits = { ns: 0.000001, us: 0.001, ms: 1, s: 1000, m: 60000, h: 3600000 };
const dayMilliseconds = 86400000;

function durationMilliseconds(value, name, allowZero = false) {
  if (typeof value !== "string" || !/^(\d+(?:\.\d+)?(?:ns|us|ms|s|m|h))+$/.test(value)) {
    throw new Error(`${name} requires an elapsed duration such as 24h`);
  }
  let result = 0;
  for (const part of value.matchAll(/(\d+(?:\.\d+)?)(ns|us|ms|s|m|h)/g)) {
    result += Number(part[1]) * durationUnits[part[2]];
  }
  if (!Number.isSafeInteger(result) || result < 0 || (!allowZero && result === 0)) {
    throw new Error(`${name} requires a positive whole number of milliseconds`);
  }
  return result;
}

export function usageRetentionSettings(override) {
  const configured = override ?? runtime.PROVIDER_USAGE_RETENTION;
  const settings = typeof configured === "string" ? JSON.parse(configured) : configured;
  if (settings === null || typeof settings !== "object" || Array.isArray(settings)) {
    throw new Error("PROVIDER_USAGE_RETENTION requires max_age, cleanup_interval, and batch_size");
  }
  const maximumAge = durationMilliseconds(settings.max_age, "PROVIDER_USAGE_RETENTION.max_age", true);
  const interval = durationMilliseconds(settings.cleanup_interval, "PROVIDER_USAGE_RETENTION.cleanup_interval");
  if (!Number.isSafeInteger(settings.batch_size) || settings.batch_size <= 0 || settings.batch_size > 10000) {
    throw new Error("PROVIDER_USAGE_RETENTION.batch_size must be between 1 and 10000");
  }
  let effectiveAge = maximumAge;
  if (maximumAge > 0) {
    for (const provider of runtime.PROVIDERS) {
      if (provider.disabled === true) {
        continue;
      }
      if (provider.daily_token_limit > 0) {
        effectiveAge = Math.max(effectiveAge, dayMilliseconds);
      }
      const windows = (provider.token_limits ?? []).map(function (limit) { return limit.window; });
      if (provider.token_limit > 0) {
        windows.push(provider.token_window);
      }
      for (const window of windows) {
        if (window?.mode === "cron") {
          throw new Error(`provider ${provider.id} uses a cron token window; set PROVIDER_USAGE_RETENTION.max_age to 0s before enabling that window`);
        }
        if (window?.mode !== "rolling" && window?.mode !== "fixed") {
          throw new Error(`provider ${provider.id} requires a valid rolling or fixed quota window for retention`);
        }
        effectiveAge = Math.max(effectiveAge, durationMilliseconds(window.duration, `provider ${provider.id} quota duration`));
      }
    }
  }
  return { maximumAge, effectiveAge, interval, batchSize: settings.batch_size };
}

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
      for (const row of rows) {
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
