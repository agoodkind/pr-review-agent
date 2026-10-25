import { DurableObject } from "cloudflare:workers";
import runtime from "../../../runtime.json" with { type: "json" };
import usageSchema from "./budget-schema.sql";
import usageHistory from "./budget-history.sql";
import usageReport from "./budget-report.sql";
import usageSnapshot from "./budget-snapshot.sql";
import usageQuery from "./budget-query.sql";
import usageSummary from "./budget-summary.sql";
import usageDetails from "./budget-details.sql";
import usageSDK from "./budget-sdk.sql";
import storageStats from "./budget-storage-stats.sql";
import { MAXIMUM_USAGE_PAGE_BYTES } from "./budget.js";
import { dayMilliseconds, maintainUsageStorage, normalizeSDKFingerprint, sdkFingerprint } from "./budget-retention.js";

const MAXIMUM_PAGE_SIZE = 500;

class UsageOverflowError extends Error {}
class UsageConflictError extends Error {}
class UsageExpiredError extends Error {}

function isNonnegativeInteger(value) {
  return Number.isSafeInteger(value) && value >= 0;
}

function validSDKUsage(usage) {
  if (usage === undefined || usage === null) {
    return true;
  }
  if (typeof usage !== "object" || Array.isArray(usage) ||
      typeof usage.api !== "string" || usage.api.trim() === "" ||
      typeof usage.usage_json !== "string" ||
      (usage.model !== undefined && typeof usage.model !== "string") ||
      (usage.response_id !== undefined && typeof usage.response_id !== "string")) {
    return false;
  }
  try {
    const native = JSON.parse(usage.usage_json);
    return native !== null && typeof native === "object" && !Array.isArray(native);
  } catch {
    return false;
  }
}

function budgetKey(prefix, day, providerId, model) {
  return `${prefix}:${day}:${providerId}${model === undefined ? "" : `:${model}`}`;
}

async function migrateLegacyUsage(transaction, day, providerId, model) {
  if (model === undefined || !runtime.PROVIDERS.some(function (provider) {
    return provider.id === providerId && provider.model === model;
  })) {
    return;
  }
  const legacyKey = budgetKey("reported_tokens", day, providerId);
  const legacyUsed = await transaction.get(legacyKey);
  if (legacyUsed === undefined) {
    return;
  }
  const modelKey = budgetKey("reported_tokens", day, providerId, model);
  const modelUsed = (await transaction.get(modelKey)) ?? 0;
  await transaction.put(modelKey, modelUsed + legacyUsed);
  await transaction.delete(legacyKey);
}

const durationUnits = { ns: 0.000001, us: 0.001, ms: 1, s: 1000, m: 60000, h: 3600000 };

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

function usageRetentionSettings(override) {
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

export class ProviderBudget extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.ctx.storage.sql.exec(usageSchema);
    this.retention = usageRetentionSettings(env.PROVIDER_USAGE_RETENTION);
    this.ctx.blockConcurrencyWhile(() => this.ensureMaintenance());
  }

  async ensureMaintenance() {
    const migrated = await this.ctx.storage.get("usage_sdk_digest_migration_version");
    if ((this.retention.maximumAge > 0 || migrated !== 1) && await this.ctx.storage.getAlarm() === null) {
      await this.ctx.storage.setAlarm(Date.now() + this.retention.interval);
    }
  }

  async alarm() {
    if (await maintainUsageStorage(this.ctx.storage, this.retention, Date.now())) {
      await this.ctx.storage.setAlarm(Date.now() + this.retention.interval);
    }
  }

  async storageStats() {
    return Response.json({ ...this.ctx.storage.sql.exec(storageStats).one(),
      database_bytes: this.ctx.storage.sql.databaseSize,
      retention: { enabled: this.retention.maximumAge > 0,
        configured_max_age_ms: this.retention.maximumAge,
        effective_max_age_ms: this.retention.effectiveAge,
        cleanup_interval_ms: this.retention.interval, batch_size: this.retention.batchSize,
        cutoff_ms: (await this.ctx.storage.get("usage_retention_cutoff_ms")) ?? 0,
        next_maintenance_ms: await this.ctx.storage.getAlarm(),
        sdk_migration_complete: await this.ctx.storage.get("usage_sdk_digest_migration_version") === 1 },
    });
  }

  async queryUsage(payload) {
    const { provider_id: providerId, model, start_ms: start, end_ms: end,
      include_start: includeStart, include_end: includeEnd, after_sequence: after,
      snapshot_sequence: requestedSnapshot, page_size: pageSize } = payload;
    if (!isNonnegativeInteger(start) || !isNonnegativeInteger(end) || start > end ||
        typeof includeStart !== "boolean" || typeof includeEnd !== "boolean" ||
        !isNonnegativeInteger(after) || !isNonnegativeInteger(requestedSnapshot) ||
        !Number.isSafeInteger(pageSize) || pageSize <= 0 || pageSize > MAXIMUM_PAGE_SIZE ||
        (requestedSnapshot === 0 && after !== 0) ||
        (requestedSnapshot !== 0 && after > requestedSnapshot)) {
      return new Response("invalid usage query", { status: 400 });
    }
    const latest = this.ctx.storage.sql.exec(usageSnapshot).one().sequence;
    if (requestedSnapshot > latest) {
      return new Response("invalid usage snapshot", { status: 400 });
    }
    const snapshot = requestedSnapshot === 0 ? latest : requestedSnapshot;
    const initialHistoryStart = this.ctx.storage.sql.exec(usageHistory, providerId, Date.now())
      .one().history_start_ms;
    const historyStart = Math.max(initialHistoryStart, (await this.ctx.storage.get("usage_retention_cutoff_ms")) ?? 0);
    const rows = this.ctx.storage.sql.exec(usageQuery, providerId, model, after, snapshot,
      start, Number(includeStart), start, end, Number(includeEnd), end, pageSize + 1)
      .toArray();
    const page = { events: [], snapshot_sequence: snapshot,
      next_sequence: 0, history_start_ms: historyStart };
    const encoder = new TextEncoder();
    let eventBytes = 0;
    for (const row of rows.slice(0, pageSize)) {
      const event = { ...row, sdk_usage: row.sdk_usage === null ? null : JSON.parse(row.sdk_usage) };
      const next = rows.length > page.events.length + 1 ? row.sequence : 0;
      const size = encoder.encode(JSON.stringify(event)).byteLength;
      const metadataSize = encoder.encode(JSON.stringify({ ...page, events: [], next_sequence: next })).byteLength;
      if (metadataSize + eventBytes + size + page.events.length > MAXIMUM_USAGE_PAGE_BYTES) {
        if (page.events.length === 0) {
          return new Response("stored usage event exceeds PROVIDER_USAGE_PAGE_BYTES", { status: 413 });
        }
        break;
      }
      eventBytes += size;
      page.events.push(event);
    }
    page.next_sequence = rows.length > page.events.length ? page.events.at(-1).sequence : 0;
    return Response.json(page);
  }

  async summarizeUsage(payload) {
    const { provider_id: providerId, start_ms: start, end_ms: end,
      include_start: includeStart, include_end: includeEnd } = payload;
    if (!isNonnegativeInteger(start) || !isNonnegativeInteger(end) || start > end ||
        typeof includeStart !== "boolean" || typeof includeEnd !== "boolean") {
      return new Response("invalid usage summary", { status: 400 });
    }
    const snapshot = this.ctx.storage.sql.exec(usageSnapshot).one().sequence;
    const initialHistoryStart = this.ctx.storage.sql.exec(usageHistory, providerId, Date.now()).one().history_start_ms;
    const historyStart = Math.max(initialHistoryStart, (await this.ctx.storage.get("usage_retention_cutoff_ms")) ?? 0);
    const groups = this.ctx.storage.sql.exec(usageSummary, providerId, snapshot,
      start + Number(!includeStart), end - Number(!includeEnd), MAXIMUM_PAGE_SIZE + 1).toArray();
    if (groups.length > MAXIMUM_PAGE_SIZE) {
      return new Response("usage summary exceeds the group limit", { status: 413 });
    }
    for (const group of groups) {
      for (const name of ["requests", "input_tokens", "output_tokens", "total_tokens", "cached_input_tokens", "cache_write_tokens"]) {
        if (!isNonnegativeInteger(group[name])) {
          return new Response("usage summary exceeds the safe integer range", { status: 413 });
        }
      }
      group.cache_details_known = Boolean(group.cache_details_known);
    }
    const result = { groups, snapshot_sequence: snapshot, history_start_ms: historyStart };
    const body = JSON.stringify(result);
    if (new TextEncoder().encode(body).byteLength > MAXIMUM_USAGE_PAGE_BYTES) {
      return new Response("usage summary exceeds the response byte limit", { status: 413 });
    }
    return new Response(body, { headers: { "Content-Type": "application/json" } });
  }

  async reportUsage(payload) {
    const { provider_id: providerId, model, admitted_at_ms: admittedAt,
      input_tokens: inputTokens, output_tokens: outputTokens, total_tokens: totalTokens,
      legacy_day: legacyDay, legacy_tokens: legacyTokens } = payload;
    const hasLegacyDay = Object.hasOwn(payload, "legacy_day");
    const hasLegacyTokens = Object.hasOwn(payload, "legacy_tokens");
    const reportId = payload.report_id;
    const cachedInput = payload.cached_input_tokens;
    const cacheWrites = payload.cache_write_tokens;
    const serviceTier = payload.service_tier;
    const sdkUsage = payload.sdk_usage;
    if (!validSDKUsage(sdkUsage)) {
      return new Response("invalid SDK usage", { status: 400 });
    }
    if ((cachedInput !== undefined && (!isNonnegativeInteger(cachedInput) || cachedInput > inputTokens)) ||
        (cacheWrites !== undefined && (!isNonnegativeInteger(cacheWrites) || cacheWrites > inputTokens)) ||
        (cachedInput !== undefined && cacheWrites !== undefined && cachedInput > inputTokens - cacheWrites) ||
        (serviceTier !== undefined && (typeof serviceTier !== "string" || serviceTier.length > 200))) {
      return new Response("invalid usage details", { status: 400 });
    }
    if (reportId !== undefined && (typeof reportId !== "string" ||
        !/^[A-Za-z0-9_-]{1,128}$/.test(reportId))) {
      return new Response("invalid report identity", { status: 400 });
    }
    if (!isNonnegativeInteger(admittedAt) || !isNonnegativeInteger(inputTokens) ||
        !isNonnegativeInteger(outputTokens) || !isNonnegativeInteger(totalTokens) ||
        hasLegacyDay !== hasLegacyTokens || (hasLegacyDay &&
          (typeof legacyDay !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(legacyDay) ||
            !isNonnegativeInteger(legacyTokens)))) {
      return new Response("invalid usage report", { status: 400 });
    }
    const serializedSDK = JSON.stringify(sdkUsage ?? null);
    const sdkDigest = await sdkFingerprint(serializedSDK);
    try {
      await this.ctx.storage.transaction(async (transaction) => {
        const cutoff = (await transaction.get("usage_retention_cutoff_ms")) ?? 0;
        if (admittedAt < cutoff || (hasLegacyDay && Date.parse(legacyDay + "T00:00:00Z") + dayMilliseconds <= cutoff)) {
          throw new UsageExpiredError("usage report predates retained history; do not replay expired usage");
        }
        const reportKey = `usage_report:${reportId}`;
        const fingerprint = JSON.stringify([providerId, model, admittedAt,
          inputTokens, outputTokens, totalTokens, legacyDay, legacyTokens]);
        const detailsKey = `usage_report_details:${reportId}`;
        const detailsFingerprint = JSON.stringify([cachedInput, cacheWrites, serviceTier]);
        const sdkKey = `usage_report_sdk:${reportId}`;
        if (reportId !== undefined) {
          const previous = await transaction.get(reportKey);
          if (previous !== undefined) {
            if (previous !== fingerprint) {
              throw new UsageConflictError("report identity already contains different usage");
            }
            const previousDetails = await transaction.get(detailsKey);
            if (previousDetails !== undefined && previousDetails !== detailsFingerprint) {
              throw new UsageConflictError("report identity already contains different usage details");
            }
            const previousSDK = (await transaction.get(sdkKey)) ?? "null";
            if (await normalizeSDKFingerprint(previousSDK) !== sdkDigest) {
              throw new UsageConflictError("report identity already contains different SDK usage");
            }
            if (previousSDK !== sdkDigest) {
              await transaction.put(sdkKey, sdkDigest);
            }
            return;
          }
        }
        this.ctx.storage.sql.exec(usageHistory, providerId, Date.now()).toArray();
        if (hasLegacyDay) {
          await migrateLegacyUsage(transaction, legacyDay, providerId, model);
          const key = budgetKey("reported_tokens", legacyDay, providerId, model);
          const used = (await transaction.get(key)) ?? 0;
          if (!Number.isSafeInteger(used + legacyTokens)) {
            throw new UsageOverflowError("daily usage exceeds the safe integer range");
          }
          await transaction.put(key, used + legacyTokens);
        }
        const inserted = this.ctx.storage.sql.exec(usageReport, providerId, model, admittedAt,
          inputTokens, outputTokens, totalTokens).one();
        this.ctx.storage.sql.exec(usageDetails, inserted.sequence,
          cachedInput ?? null, cacheWrites ?? null, serviceTier ?? null);
        if (sdkUsage !== undefined && sdkUsage !== null) {
          this.ctx.storage.sql.exec(usageSDK, inserted.sequence, serializedSDK);
        }
        if (reportId !== undefined) {
          await transaction.put(reportKey, fingerprint);
          await transaction.put(detailsKey, detailsFingerprint);
          await transaction.put(sdkKey, sdkDigest);
        }
      });
    } catch (error) {
      if (error instanceof UsageOverflowError) {
        return new Response(error.message, { status: 400 });
      }
      if (error instanceof UsageConflictError) {
        return new Response(error.message, { status: 409 });
      }
      if (error instanceof UsageExpiredError) {
        return new Response(error.message, { status: 410 });
      }
      console.error(JSON.stringify({ message: "model usage storage failed",
        report_id: reportId, provider_id: providerId, model, error: String(error) }));
      throw error;
    }
    return Response.json({ reported: true, sdk_usage_stored: sdkUsage !== undefined && sdkUsage !== null });
  }

  async fetch(request) {
    if (request.method !== "POST") {
      return new Response("not found", { status: 404 });
    }
    const pathname = new URL(request.url).pathname;
    if (pathname === "/ensure_maintenance") {
      await this.ensureMaintenance();
      return Response.json({ scheduled: true });
    }
    if (pathname === "/storage_stats") {
      return this.storageStats();
    }
    if (pathname !== "/check" && pathname !== "/report" && pathname !== "/reserve" &&
        pathname !== "/query_usage" && pathname !== "/report_usage" && pathname !== "/summarize_usage") {
      return new Response("not found", { status: 404 });
    }
    const payload = await request.json();
    const { provider_id: providerId, model, tokens, limit, day: reportedDay } = payload;
    if (typeof providerId !== "string" || !/^[a-z][a-z0-9_]*$/.test(providerId)) {
      return new Response("invalid provider", { status: 400 });
    }
    if (model !== undefined && (typeof model !== "string" || model.length === 0 || model.length > 200)) {
      return new Response("invalid model", { status: 400 });
    }
    if (pathname === "/query_usage" || pathname === "/report_usage" || pathname === "/summarize_usage") {
      if (model === undefined && pathname !== "/summarize_usage") {
        return new Response("invalid model", { status: 400 });
      }
      if (pathname === "/summarize_usage") {
        return this.summarizeUsage(payload);
      }
      return pathname === "/query_usage" ? this.queryUsage(payload) : this.reportUsage(payload);
    }
    const day = new Date().toISOString().slice(0, 10);
    if (pathname === "/reserve") {
      if (!Number.isSafeInteger(tokens) || tokens <= 0 ||
          !Number.isSafeInteger(limit) || limit <= 0) {
        return new Response("invalid reservation", { status: 400 });
      }
      const key = budgetKey("tokens", day, providerId, model);
      const allowed = await this.ctx.storage.transaction(async function (transaction) {
        const reserved = (await transaction.get(key)) ?? 0;
        if (reserved + tokens > limit) {
          return false;
        }
        await transaction.put(key, reserved + tokens);
        return true;
      });
      return Response.json({ allowed });
    }
    if (pathname === "/check") {
      if (!Number.isSafeInteger(limit) || limit <= 0) {
        return new Response("invalid limit", { status: 400 });
      }
      const used = await this.ctx.storage.transaction(async (transaction) => {
        await migrateLegacyUsage(transaction, day, providerId, model);
        return (await transaction.get(budgetKey("reported_tokens", day, providerId, model))) ?? 0;
      });
      return Response.json({ allowed: used < limit, day, used, limit, remaining: Math.max(0, limit - used) });
    }
    if (!Number.isSafeInteger(tokens) || tokens <= 0 ||
        typeof reportedDay !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(reportedDay)) {
      return new Response("invalid usage", { status: 400 });
    }
    const key = budgetKey("reported_tokens", reportedDay, providerId, model);
    const accepted = await this.ctx.storage.transaction(async function (transaction) {
      const cutoff = (await transaction.get("usage_retention_cutoff_ms")) ?? 0;
      if (Date.parse(reportedDay + "T00:00:00Z") + dayMilliseconds <= cutoff) {
        return false;
      }
      await migrateLegacyUsage(transaction, reportedDay, providerId, model);
      const used = (await transaction.get(key)) ?? 0;
      await transaction.put(key, used + tokens);
      return true;
    });
    if (!accepted) {
      return new Response("usage report predates retained history; do not replay expired usage", { status: 410 });
    }
    return Response.json({ reported: true });
  }
}
