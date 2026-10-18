import { env, WorkerEntrypoint } from "cloudflare:workers";
import runtime from "../../../runtime.json" with { type: "json" };

import { createPrAgentEnvironment } from "./configuration.js";
import { ProviderBudget } from "./budget-do.js";
import { ensureUsageMaintenance } from "./budget.js";
import { ReviewContainer } from "./container.js";
import { WebhookReplayQueue } from "./replay.js";
import { routeRequest } from "./router.js";
import { scheduledReassessment } from "./reassessment.js";

export { WebhookReplayQueue };
export { ProviderBudget };

export class PrAgentContainer extends ReviewContainer {
  defaultPort = Number(runtime.PORT);
  sleepAfter = runtime.CONTAINER_SLEEP_AFTER;
  envVars = createPrAgentEnvironment(env);

  async stopForHandoff() {
    if (this.env.REVIEW_HANDOFF !== "true") {
      throw new Error("legacy review service can stop only during deployment handoff");
    }
    await this.pauseForHandoff();
    if (this.forwardingRequests === 0 && this.ctx.container.running &&
        await this.ctx.storage.get("review_handoff_stop_requested") !== true) {
      await this.stop();
      await this.ctx.storage.put("review_handoff_stop_requested", true);
    }
    return this.handoffStatus();
  }

}

export class RuntimeConfiguration extends WorkerEntrypoint {
  getConfiguration() {
    return { environment: createPrAgentEnvironment(this.env), port: Number(runtime.PORT),
      sleepAfter: runtime.CONTAINER_SLEEP_AFTER };
  }
}

function scheduled(controller, env, context) {
  scheduledReassessment(controller, env, context);
  context.waitUntil(ensureUsageMaintenance(env));
}

export default { fetch: routeRequest, scheduled };
