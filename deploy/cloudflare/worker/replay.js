import { DurableObject } from "cloudflare:workers";

import {
  dueEntries,
  entryFromDelivery,
  nextWakeAt,
  replayDelayMs,
  shouldAbandon,
} from "./replaylogic.js";

// WebhookReplayQueue holds webhook deliveries the container could not take and
// replays them until it can.
//
// A webhook GitHub delivers is delivered once: GitHub marks a failed delivery
// and does not send it again. When the container is unavailable, dropping the
// delivery means the pull request never gets its check, which blocks it with
// nothing a person can point at. The queue accepts the delivery on the
// container's behalf and replays it with backoff once the container answers.
// Replaying a delivery the container did process is safe, because the service
// suppresses duplicate delivery identifiers and already reviewed heads.
export class WebhookReplayQueue extends DurableObject {
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method !== "POST" || url.pathname !== "/enqueue") {
      return new Response("not found", { status: 404 });
    }
    const entry = await request.json();
    await this.ctx.storage.put(entryKey(entry.id), entry);
    console.log(
      JSON.stringify({
        message: "webhook queued for replay",
        deliveryId: entry.id,
        notBefore: entry.notBefore,
      }),
    );
    await this.armAlarm();
    return Response.json({ queued: entry.id });
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
    if (shouldAbandon(entry, now)) {
      console.error(
        JSON.stringify({
          message: "webhook replay abandoned",
          deliveryId: entry.id,
          attempts: entry.attempts,
          firstSeen: entry.firstSeen,
        }),
      );
      await this.ctx.storage.delete(entryKey(entry.id));
      return;
    }

    let status = 0;
    try {
      const container = this.env.PR_AGENT.getByName("github-app");
      const response = await container.fetch(
        new Request(`https://replay${entry.path}`, {
          method: "POST",
          headers: entry.headers,
          body: entry.body,
        }),
      );
      status = response.status;
    } catch (error) {
      console.error(
        JSON.stringify({
          message: "webhook replay attempt failed",
          deliveryId: entry.id,
          attempts: entry.attempts,
          error: String(error),
        }),
      );
    }

    // The Go service never answers 500, so 500 is the container library
    // answering for a container that was not there. Anything else means the
    // service itself spoke, and the delivery is done whether it was accepted
    // or refused.
    if (status !== 0 && status !== 500) {
      console.log(
        JSON.stringify({
          message: "webhook replayed",
          deliveryId: entry.id,
          attempts: entry.attempts,
          status,
        }),
      );
      await this.ctx.storage.delete(entryKey(entry.id));
      return;
    }

    entry.attempts += 1;
    entry.notBefore = now + replayDelayMs(entry.attempts);
    await this.ctx.storage.put(entryKey(entry.id), entry);
  }

  async armAlarm() {
    const entries = [...(await this.ctx.storage.list({ prefix: KEY_PREFIX })).values()];
    const wakeAt = nextWakeAt(entries);
    if (wakeAt === null) {
      return;
    }
    await this.ctx.storage.setAlarm(Math.max(wakeAt, Date.now() + 1000));
  }
}

const KEY_PREFIX = "delivery:";

function entryKey(id) {
  return KEY_PREFIX + id;
}
