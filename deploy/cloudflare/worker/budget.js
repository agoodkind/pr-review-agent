import { readBoundedText } from "./servicelogs.js";

export const BUDGET_PATH = "/internal/v1/provider_budget";
export const BUDGET_SIGNATURE_HEADER = "X-Pr-Agent-Budget-Signature";
const MAXIMUM_RESERVATION_BYTES = 1024;

export async function handleProviderBudget(request, env, providers, verifySignature) {
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
  let reservation;
  try {
    reservation = JSON.parse(body);
  } catch {
    return new Response("invalid reservation", { status: 400 });
  }
  const provider = providers.find(function (candidate) {
    return candidate.id === reservation.provider_id;
  });
  if (!provider || !Number.isSafeInteger(provider.daily_token_limit) || provider.daily_token_limit <= 0 ||
      !Number.isSafeInteger(reservation.tokens) || reservation.tokens <= 0) {
    return new Response("invalid reservation", { status: 400 });
  }
  const budget = env.PROVIDER_BUDGET.getByName("provider-budgets");
  return budget.fetch(new Request("https://budget/reserve", {
    method: "POST",
    body: JSON.stringify({
      providerId: provider.id,
      tokens: reservation.tokens,
      limit: provider.daily_token_limit,
    }),
  }));
}
