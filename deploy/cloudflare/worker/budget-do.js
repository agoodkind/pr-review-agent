import { DurableObject } from "cloudflare:workers";
import runtime from "../../../runtime.json" with { type: "json" };
import usageSchema from "./budget-schema.sql";
import usageHistory from "./budget-history.sql";
import usageReport from "./budget-report.sql";
import usageSnapshot from "./budget-snapshot.sql";
import usageQuery from "./budget-query.sql";

const MAXIMUM_PAGE_SIZE = 500;

class UsageOverflowError extends Error {}

function isNonnegativeInteger(value) {
  return Number.isSafeInteger(value) && value >= 0;
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

export class ProviderBudget extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.ctx.storage.sql.exec(usageSchema);
  }

  queryUsage(payload) {
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
    const historyStart = this.ctx.storage.sql.exec(usageHistory, providerId, Date.now())
      .one().history_start_ms;
    const rows = this.ctx.storage.sql.exec(usageQuery, providerId, model, after, snapshot,
      start, Number(includeStart), start, end, Number(includeEnd), end, pageSize + 1)
      .toArray();
    const hasNext = rows.length > pageSize;
    const events = rows.slice(0, pageSize);
    return Response.json({ events, snapshot_sequence: snapshot,
      next_sequence: hasNext ? events.at(-1).sequence : 0, history_start_ms: historyStart });
  }

  async reportUsage(payload) {
    const { provider_id: providerId, model, admitted_at_ms: admittedAt,
      input_tokens: inputTokens, output_tokens: outputTokens, total_tokens: totalTokens,
      legacy_day: legacyDay, legacy_tokens: legacyTokens } = payload;
    const hasLegacyDay = Object.hasOwn(payload, "legacy_day");
    const hasLegacyTokens = Object.hasOwn(payload, "legacy_tokens");
    if (!isNonnegativeInteger(admittedAt) || !isNonnegativeInteger(inputTokens) ||
        !isNonnegativeInteger(outputTokens) || !isNonnegativeInteger(totalTokens) ||
        hasLegacyDay !== hasLegacyTokens || (hasLegacyDay &&
          (typeof legacyDay !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(legacyDay) ||
            !isNonnegativeInteger(legacyTokens)))) {
      return new Response("invalid usage report", { status: 400 });
    }
    try {
      await this.ctx.storage.transaction(async (transaction) => {
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
        this.ctx.storage.sql.exec(usageReport, providerId, model, admittedAt,
          inputTokens, outputTokens, totalTokens);
      });
    } catch (error) {
      if (error instanceof UsageOverflowError) {
        return new Response(error.message, { status: 400 });
      }
      throw error;
    }
    return Response.json({ reported: true });
  }

  async fetch(request) {
    if (request.method !== "POST") {
      return new Response("not found", { status: 404 });
    }
    const pathname = new URL(request.url).pathname;
    if (pathname !== "/check" && pathname !== "/report" && pathname !== "/reserve" &&
        pathname !== "/query_usage" && pathname !== "/report_usage") {
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
    if (pathname === "/query_usage" || pathname === "/report_usage") {
      if (model === undefined) {
        return new Response("invalid model", { status: 400 });
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
    await this.ctx.storage.transaction(async function (transaction) {
      await migrateLegacyUsage(transaction, reportedDay, providerId, model);
      const used = (await transaction.get(key)) ?? 0;
      await transaction.put(key, used + tokens);
    });
    return Response.json({ reported: true });
  }
}
