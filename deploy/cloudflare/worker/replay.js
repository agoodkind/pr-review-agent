import { DurableObject } from "cloudflare:workers";
import runtime from "../../../runtime.json" with { type: "json" };

import { deliverySettled, dueEntries, isOverdue, nextWakeAt, replayDelayMs } from "./replaylogic.js";
import { REASSESSMENT_SIGNATURE, reassessmentSignature } from "./reassessment.js";
import { REVIEW_SETTINGS_HEADER, REVIEW_SETTINGS_SIGNATURE_HEADER, createReviewSettingsHeader, signReviewSettings } from "./configuration.js";

const KEY_PREFIX = "delivery:";
const REASSESSMENT_PREFIX = "reassessment:";

function entryKey(id) {
  return KEY_PREFIX + id;
}

export class WebhookReplayQueue extends DurableObject {
  async fetch(request) {
    const path = new URL(request.url).pathname;
    if (request.method !== "POST") {
      return new Response("not found", { status: 404 });
    }
    if (path === "/reassessments") {
      return this.reassessmentMutation(await request.json());
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
    const records = await this.ctx.storage.list({ prefix: REASSESSMENT_PREFIX });
    let dispatched = 0;
    for (const [key, record] of records) {
      if (record.phase === "terminal") {
        if (record.retain_until_ms <= now) {
          await this.ctx.storage.transaction(async function expire(transaction) {
            const current = await transaction.get(key);
            if (current?.version === record.version && current.phase === "terminal") {
              await transaction.delete(key);
            }
          });
        }
      } else if (record.not_before_ms <= now && dispatched < Number(runtime.REVIEW_WORKERS)) {
        dispatched += 1;
        await this.reassessOne(record);
      }
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
    let wakeAt = nextWakeAt(entries);
    const records = [...(await this.ctx.storage.list({ prefix: REASSESSMENT_PREFIX })).values()];
    for (const record of records) {
      const due = record.phase === "terminal" ? record.retain_until_ms : record.not_before_ms;
      if (wakeAt === null || due < wakeAt) {
        wakeAt = due;
      }
    }
    if (wakeAt === null) {
      await this.ctx.storage.deleteAlarm();
      return;
    }
    await this.ctx.storage.setAlarm(Math.max(wakeAt, Date.now() + 1000));
  }

  async reassessmentMutation(payload) {
    if (payload.action === "query") {
      if (payload.key === "") {
        const records = [...(await this.ctx.storage.list({ prefix: REASSESSMENT_PREFIX })).values()];
        return Response.json({ applied: false, record: null, records });
      }
      const record = await this.ctx.storage.get(REASSESSMENT_PREFIX + payload.key);
      return Response.json({ applied: false, record: record ?? null });
    }
    if (payload.action !== "transition" || typeof payload.key !== "string" || payload.key === "" || !payload.record || payload.record.key !== payload.key || !Number.isSafeInteger(payload.expected_version)) {
      return new Response("invalid transition", { status: 400 });
    }
    const result = await this.ctx.storage.transaction(async function transition(transaction) {
      const key = REASSESSMENT_PREFIX + payload.key;
      const current = await transaction.get(key);
      if ((current?.version ?? 0) !== payload.expected_version) {
        return { applied: false, record: current ?? null };
      }
      const record = { ...payload.record, version: payload.expected_version + 1 };
      await transaction.put(key, record);
      return { applied: true, record };
    });
    await this.armAlarm();
    return Response.json(result);
  }

  async callback(action, record) {
    const body = JSON.stringify({ action, record });
    const signature = await reassessmentSignature(this.env.GITHUB_WEBHOOK_SECRET, body);
    const settings = createReviewSettingsHeader();
    const settingsSignature = await signReviewSettings(this.env.GITHUB_WEBHOOK_SECRET, settings, body);
    return this.env.PR_AGENT.getByName("github-app").fetch(new Request("https://reassessment/internal/v1/reassessment", {
      method: "POST", headers: { "Content-Type": "application/json", [REASSESSMENT_SIGNATURE]: signature, [REVIEW_SETTINGS_HEADER]: settings, [REVIEW_SETTINGS_SIGNATURE_HEADER]: settingsSignature }, body,
    }));
  }

  async reassessOne(record) {
    try {
      const response = await this.callback("plan", record);
      if (!response.ok) {
        throw new Error(`callback HTTP ${response.status}`);
      }
      const planned = await response.json();
      const persisted = await this.reassessmentMutation({ action: "transition", key: record.key, expected_version: record.version, record: planned });
      const result = await persisted.json();
      if (!result.applied) {
        return;
      }
      if (result.record.phase === "terminal") {
        const reason = result.record.reason;
        const message = reason === "completed" ? "reassessment completed" : (reason === "expired" || reason === "attempts_exhausted" ? "reassessment exhausted" : "reassessment cancelled");
        console.log(JSON.stringify({ message, reassessment_id: result.record.id, generation: result.record.generation, origin_delivery_id: result.record.origin_delivery_id, attempt_id: result.record.attempt_id, repository: `${result.record.job.Repository.Owner}/${result.record.job.Repository.Name}`, pull_request: result.record.job.Number, head: result.record.head, phase: result.record.phase, reason, attempts: result.record.attempts }));
      } else if (!result.record.dispatch) {
        console.log(JSON.stringify({ message: "reassessment deferred", reassessment_id: result.record.id, generation: result.record.generation, origin_delivery_id: result.record.origin_delivery_id, attempt_id: result.record.attempt_id, reason: result.record.reason, not_before_ms: result.record.not_before_ms, attempts: result.record.attempts }));
      }
      if (result.record.phase !== "running" || !result.record.dispatch) {
        return;
      }
      const executed = await this.callback("execute", result.record);
      console.log(JSON.stringify({ message: "reassessment callback", reassessment_id: record.id, attempt_id: result.record.attempt_id, status: executed.status }));
    } catch {
      console.error(JSON.stringify({ message: "reassessment callback failed", reassessment_id: record.id, attempt_id: record.attempt_id }));
      const current = await this.ctx.storage.get(REASSESSMENT_PREFIX + record.key);
      if (current?.version === record.version) {
        current.not_before_ms = Date.now() + replayDelayMs(5);
        current.version += 1;
        await this.ctx.storage.put(REASSESSMENT_PREFIX + record.key, current);
      }
    }
  }
}
