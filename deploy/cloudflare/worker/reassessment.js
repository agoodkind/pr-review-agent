import { verifyServiceLogSignature } from "./servicelogs.js";
import { authenticateOperator } from "./operator.js";

export const REASSESSMENT_PATH = "/internal/v1/reassessments";
export const REASSESSMENT_SIGNATURE = "X-Pr-Agent-Reassessment-Signature";

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

export async function reassessmentSignature(secret, body) {
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const digest = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(body));
  return "sha256=" + [...new Uint8Array(digest)].map(function hex(byte) { return byte.toString(16).padStart(2, "0"); }).join("");
}
