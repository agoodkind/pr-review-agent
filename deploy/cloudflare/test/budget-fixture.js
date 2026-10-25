import { ProviderBudget } from "../worker/budget-do.js";
import { routeRequest } from "../worker/router.js";
import runtime from "../../../runtime.json" with { type: "json" };

runtime.PROVIDERS.push({
  id: "retention_test",
  model: "retention_model",
  daily_token_limit: 2_000_000,
  token_limits: [
    { limit: 0, window: { mode: "rolling", duration: "168h" } },
    { limit: 0, window: { mode: "cron", schedule: "0 0 1 * *", timezone: "UTC" } },
  ],
});

export { ProviderBudget };

export default { fetch: routeRequest };
