import { DurableObject } from "cloudflare:workers";

export class ProviderBudget extends DurableObject {
  async fetch(request) {
    if (request.method !== "POST" || new URL(request.url).pathname !== "/reserve") {
      return new Response("not found", { status: 404 });
    }
    const reservation = await request.json();
    const { providerId, tokens, limit } = reservation;
    if (typeof providerId !== "string" || !/^[a-z][a-z0-9_]*$/.test(providerId) ||
        !Number.isSafeInteger(tokens) || tokens <= 0 ||
        !Number.isSafeInteger(limit) || limit <= 0) {
      return new Response("invalid reservation", { status: 400 });
    }
    const day = new Date().toISOString().slice(0, 10);
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
}
