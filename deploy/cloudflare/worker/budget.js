import { readBoundedText } from "./servicelogs.js";
import { authenticateOperator } from "./operator.js";
import runtime from "../../../runtime.json" with { type: "json" };

export const BUDGET_PATH = "/internal/v1/provider_budget";
export const BUDGET_SIGNATURE_HEADER = "X-Pr-Agent-Budget-Signature";
const MAXIMUM_QUERY_RESPONSE_BYTES = 256 * 1024;

function requiredPositiveInteger(name) {
  const value = runtime[name];
  if ((typeof value !== "string" && typeof value !== "number") ||
      !Number.isSafeInteger(Number(value)) || Number(value) <= 0) {
    throw new Error(`${name} must be a positive integer`);
  }
  return Number(value);
}

export const MAXIMUM_USAGE_REPORT_BYTES = requiredPositiveInteger("PROVIDER_USAGE_MAX_BYTES");
export const MAXIMUM_USAGE_PAGE_BYTES = requiredPositiveInteger("PROVIDER_USAGE_PAGE_BYTES");
if (MAXIMUM_USAGE_PAGE_BYTES <= MAXIMUM_USAGE_REPORT_BYTES ||
    MAXIMUM_USAGE_PAGE_BYTES > MAXIMUM_QUERY_RESPONSE_BYTES) {
  throw new Error("PROVIDER_USAGE_PAGE_BYTES must exceed PROVIDER_USAGE_MAX_BYTES and fit the Go response byte limit");
}
const DAILY_READ_FIELDS = new Set(["provider_id", "model", "limit"]);
const USAGE_READ_FIELDS = new Set([
  "action", "provider_id", "model", "start_ms", "end_ms", "include_start", "include_end",
  "after_sequence", "snapshot_sequence", "page_size",
]);

function operatorCanRead(payload) {
  if (payload.action === "storage_stats") {
    return Object.keys(payload).length === 1;
  }
  const fields = Object.hasOwn(payload, "action") ? USAGE_READ_FIELDS : DAILY_READ_FIELDS;
  if (Object.hasOwn(payload, "action") && payload.action !== "query_usage") {
    return false;
  }
  return Object.keys(payload).every(function (key) { return fields.has(key); });
}

export async function handleProviderBudget(request, env, verifySignature) {
  if (request.method !== "POST") {
    return new Response("not found", { status: 404 });
  }
  const body = await readBoundedText(request, MAXIMUM_USAGE_REPORT_BYTES);
  if (body === null) {
    return new Response("usage request exceeds PROVIDER_USAGE_MAX_BYTES", { status: 413 });
  }
  const signed = await verifySignature(env.GITHUB_WEBHOOK_SECRET, body,
    request.headers.get(BUDGET_SIGNATURE_HEADER) ?? "");
  const operator = !signed && await authenticateOperator(request, env.OPERATOR_TOKEN_SHA256);
  if (!signed && !operator) {
    return new Response("invalid signature", { status: 401 });
  }
  const budget = env.PROVIDER_BUDGET.getByName("provider-budgets");
  let pathname = "/check";
  try {
    const payload = JSON.parse(body);
    if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
      return new Response("invalid budget request", { status: 400 });
    }
    if (operator && !operatorCanRead(payload)) {
      return new Response("operator token permits counter reads only", { status: 403 });
    }
    if (Object.hasOwn(payload, "action")) {
      if (payload.action !== "query_usage" && payload.action !== "report_usage" && payload.action !== "storage_stats") {
        return new Response("invalid budget action", { status: 400 });
      }
      pathname = `/${payload.action}`;
    } else if (Object.hasOwn(payload, "tokens")) {
      pathname = Object.hasOwn(payload, "limit") ? "/reserve" : "/report";
    }
  } catch {
    return new Response("invalid budget request", { status: 400 });
  }
  return budget.fetch(new Request(`https://budget${pathname}`, {
    method: "POST",
    body,
  }));
}

export async function ensureUsageMaintenance(env) {
  const response = await env.PROVIDER_BUDGET.getByName("provider-budgets").fetch(new Request("https://budget/ensure_maintenance", { method: "POST" }));
  if (!response.ok) {
    throw new Error("usage storage maintenance initialization failed");
  }
}
