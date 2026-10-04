import { readBoundedText } from "./servicelogs.js";
import { authenticateOperator } from "./operator.js";

export const BUDGET_PATH = "/internal/v1/provider_budget";
export const BUDGET_SIGNATURE_HEADER = "X-Pr-Agent-Budget-Signature";
const MAXIMUM_RESERVATION_BYTES = 1024;
const DAILY_READ_FIELDS = new Set(["provider_id", "model", "limit"]);
const USAGE_READ_FIELDS = new Set([
  "action", "provider_id", "model", "start_ms", "end_ms", "include_start", "include_end",
  "after_sequence", "snapshot_sequence", "page_size",
]);

function operatorCanRead(payload) {
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
  const body = await readBoundedText(request, MAXIMUM_RESERVATION_BYTES);
  if (body === null) {
    return new Response("reservation too large", { status: 413 });
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
      if (payload.action !== "query_usage" && payload.action !== "report_usage") {
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
