import { DurableObject } from "cloudflare:workers";

export class ProviderBudget extends DurableObject {
  async fetch(request) {
    if (request.method !== "POST") {
      return new Response("not found", { status: 404 });
    }
    const pathname = new URL(request.url).pathname;
    if (pathname !== "/check" && pathname !== "/report" && pathname !== "/reserve") {
      return new Response("not found", { status: 404 });
    }
    const { provider_id: providerId, tokens, limit, day: reportedDay } = await request.json();
    if (typeof providerId !== "string" || !/^[a-z][a-z0-9_]*$/.test(providerId)) {
      return new Response("invalid provider", { status: 400 });
    }
    const day = new Date().toISOString().slice(0, 10);
    if (pathname === "/reserve") {
      if (!Number.isSafeInteger(tokens) || tokens <= 0 ||
          !Number.isSafeInteger(limit) || limit <= 0) {
        return new Response("invalid reservation", { status: 400 });
      }
      const key = `tokens:${day}:${providerId}`;
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
      const used = (await this.ctx.storage.get(`reported_tokens:${day}:${providerId}`)) ?? 0;
      return Response.json({ allowed: used < limit, day });
    }
    if (!Number.isSafeInteger(tokens) || tokens <= 0 ||
        typeof reportedDay !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(reportedDay)) {
      return new Response("invalid usage", { status: 400 });
    }
    const key = `reported_tokens:${reportedDay}:${providerId}`;
    await this.ctx.storage.transaction(async function (transaction) {
      const used = (await transaction.get(key)) ?? 0;
      await transaction.put(key, used + tokens);
    });
    return Response.json({ reported: true });
  }
}
