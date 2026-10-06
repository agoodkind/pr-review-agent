import { verifyServiceLogSignature } from "./servicelogs.js";
import { authenticateOperator } from "./operator.js";
import { REVIEW_SETTINGS_HEADER, REVIEW_SETTINGS_SIGNATURE_HEADER, createReviewSettingsHeader, signReviewSettings } from "./configuration.js";

export const REASSESSMENT_PATH = "/internal/v1/reassessments";
export const REASSESSMENT_SIGNATURE = "X-Pr-Agent-Reassessment-Signature";

/** @param {Request} request @param {import("./drift.js").WorkerEnvironment} env @returns {Promise<Response>} */
export async function handleReassessments(request, env) {
  if (request.method !== "POST") {
    return new Response("method not allowed", { status: 405 });
  }
  const body = await request.text();
  const signed = await verifyServiceLogSignature(env.GITHUB_WEBHOOK_SECRET, body, request.headers.get(REASSESSMENT_SIGNATURE) ?? "");
  const operator = !signed && await authenticateOperator(request, env.OPERATOR_TOKEN_SHA256);
  if (!signed && !operator) {
    return new Response("invalid signature", { status: 401 });
  }
  let payload;
  try {
    payload = JSON.parse(body);
  } catch {
    return new Response("invalid record", { status: 400 });
  }
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) {
    return new Response("invalid record", { status: 400 });
  }
  if (operator && payload.action !== "query") {
    return new Response("read only", { status: 403 });
  }
  return env.REPLAY_QUEUE.getByName("webhook-replays").fetch(new Request("https://replay/reassessments", { method: "POST", body }));
}

/** @param {string} secret @param {string} body @returns {Promise<string>} */
export async function reassessmentSignature(secret, body) {
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const digest = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(body));
  return "sha256=" + [...new Uint8Array(digest)].map(function hex(byte) { return byte.toString(16).padStart(2, "0"); }).join("");
}

/** @param {import("./drift.js").WorkerEnvironment} env @param {Record<string, unknown>} payload @returns {Promise<Response>} */
export async function reassessmentCallback(env, payload) {
  const body = JSON.stringify(payload);
  const signature = await reassessmentSignature(env.GITHUB_WEBHOOK_SECRET, body);
  const settings = createReviewSettingsHeader();
  const settingsSignature = await signReviewSettings(env.GITHUB_WEBHOOK_SECRET, settings, body);
  return env.PR_AGENT.getByName("github-app").fetch(new Request("https://reassessment/internal/v1/reassessment", {
    method: "POST",
    headers: { "Content-Type": "application/json", [REASSESSMENT_SIGNATURE]: signature, [REVIEW_SETTINGS_HEADER]: settings, [REVIEW_SETTINGS_SIGNATURE_HEADER]: settingsSignature },
    body,
  }));
}

/** @param {ScheduledController} controller @param {import("./drift.js").WorkerEnvironment} env @param {ExecutionContext} context @returns {void} */
export function scheduledReassessment(controller, env, context) {
  context.waitUntil(ensureReassessmentSchedule(env));
}

/** @param {import("./drift.js").WorkerEnvironment} env @returns {Promise<void>} */
async function ensureReassessmentSchedule(env) {
  const response = await env.REPLAY_QUEUE.getByName("webhook-replays").fetch(new Request("https://replay/reassessments", {
    method: "POST", body: JSON.stringify({ action: "ensure_sweep" }),
  }));
  if (!response.ok) {
    throw new Error("reassessment schedule initialization failed");
  }
}
