import { DurableObject } from "cloudflare:workers";

import { deliverySettled, dueEntries, isOverdue, nextWakeAt, replayDelayMs } from "./replaylogic.js";

const KEY_PREFIX = "delivery:";

function entryKey(id) {
  return KEY_PREFIX + id;
}

export class WebhookReplayQueue extends DurableObject {
  async fetch(request) {
    const path = new URL(request.url).pathname;
    if (request.method !== "POST") {
      return new Response("not found", { status: 404 });
    }

    if (path === "/enqueue") {
      const entry = await request.json();
      if (typeof entry.id !== "string" || entry.id === "") {
        return new Response("invalid delivery", { status: 400 });
      }
      await this.ctx.storage.transaction(async (transaction) => {
        const key = entryKey(entry.id);
        if ((await transaction.get(key)) === undefined) {
          await transaction.put(key, entry);
        }
      });
      await this.armAlarm();
      return Response.json({ queued: entry.id });
    }

    if (path === "/settle") {
      const { id } = await request.json();
      if (typeof id !== "string" || id === "") {
        return new Response("invalid delivery", { status: 400 });
      }
      await this.ctx.storage.delete(entryKey(id));
      return Response.json({ settled: id });
    }

    return new Response("not found", { status: 404 });
  }

  async alarm() {
    const now = Date.now();
    const entries = [...(await this.ctx.storage.list({ prefix: KEY_PREFIX })).values()];
    for (const entry of dueEntries(entries, now)) {
      await this.replayOne(entry, now);
    }
    await this.armAlarm();
  }

  async replayOne(entry, now) {
    if (isOverdue(entry, now) && !entry.overdueLogged) {
      console.error(JSON.stringify({ message: "webhook review overdue", deliveryId: entry.id, firstSeen: entry.firstSeen }));
      entry.overdueLogged = true;
    }

    let response = null;
    try {
      const container = this.env.PR_AGENT.getByName("github-app");
      response = await container.fetch(new Request(`https://replay${entry.path}`, {
        method: "POST",
        headers: entry.headers,
        body: entry.body,
      }));
    } catch (error) {
      console.error(JSON.stringify({ message: "webhook replay failed", deliveryId: entry.id, error: String(error) }));
    }

    if (deliverySettled(response)) {
      await this.ctx.storage.delete(entryKey(entry.id));
      console.log(JSON.stringify({ message: "webhook review settled", deliveryId: entry.id, status: response.status }));
      return;
    }

    await this.ctx.storage.transaction(async (transaction) => {
      const key = entryKey(entry.id);
      const current = await transaction.get(key);
      if (current === undefined) {
        return;
      }
      current.attempts += 1;
      current.notBefore = now + replayDelayMs(current.attempts);
      current.overdueLogged = entry.overdueLogged;
      await transaction.put(key, current);
    });
  }

  async armAlarm() {
    const entries = [...(await this.ctx.storage.list({ prefix: KEY_PREFIX })).values()];
    const wakeAt = nextWakeAt(entries);
    if (wakeAt === null) {
      await this.ctx.storage.deleteAlarm();
      return;
    }
    await this.ctx.storage.setAlarm(Math.max(wakeAt, Date.now() + 1000));
  }
}
