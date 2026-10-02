import { DurableObject } from "cloudflare:workers";
import runtime from "../../../runtime.json" with { type: "json" };

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
  async fetch(request) {
    if (request.method !== "POST") {
      return new Response("not found", { status: 404 });
    }
    const pathname = new URL(request.url).pathname;
    if (pathname !== "/check" && pathname !== "/report" && pathname !== "/reserve") {
      return new Response("not found", { status: 404 });
    }
    const { provider_id: providerId, model, tokens, limit, day: reportedDay } = await request.json();
    if (typeof providerId !== "string" || !/^[a-z][a-z0-9_]*$/.test(providerId)) {
      return new Response("invalid provider", { status: 400 });
    }
    if (model !== undefined && (typeof model !== "string" || model.length === 0 || model.length > 200)) {
      return new Response("invalid model", { status: 400 });
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
