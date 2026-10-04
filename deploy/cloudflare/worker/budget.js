import { readBoundedText } from "./servicelogs.js";

export const BUDGET_PATH = "/internal/v1/provider_budget";
export const BUDGET_SIGNATURE_HEADER = "X-Pr-Agent-Budget-Signature";
const MAXIMUM_RESERVATION_BYTES = 1024;

export async function handleProviderBudget(request, env, verifySignature) {
  if (request.method !== "POST") {
    return new Response("not found", { status: 404 });
  }
  const body = await readBoundedText(request, MAXIMUM_RESERVATION_BYTES);
  if (body === null) {
    return new Response("reservation too large", { status: 413 });
  }
  if (!(await verifySignature(env.GITHUB_WEBHOOK_SECRET, body, request.headers.get(BUDGET_SIGNATURE_HEADER) ?? ""))) {
    return new Response("invalid signature", { status: 401 });
  }
  const budget = env.PROVIDER_BUDGET.getByName("provider-budgets");
  let pathname = "/check";
  try {
    const payload = JSON.parse(body);
    if (payload === null || typeof payload !== "object" || Array.isArray(payload)) {
      return new Response("invalid budget request", { status: 400 });
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
