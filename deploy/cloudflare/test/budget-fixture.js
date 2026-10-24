import { ProviderBudget } from "../worker/budget-do.js";
import { routeRequest } from "../worker/router.js";
import runtime from "../../../runtime.json" with { type: "json" };

runtime.PROVIDERS.push({ id: "retention_test", model: "retention_model", daily_token_limit: 2_000_000 });

export { ProviderBudget };

export default { fetch: routeRequest };
