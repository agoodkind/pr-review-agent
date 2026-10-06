import { Container } from "@cloudflare/containers";
import { env } from "cloudflare:workers";
import runtime from "../../../runtime.json" with { type: "json" };

import { createPrAgentEnvironment } from "./configuration.js";
import { ProviderBudget } from "./budget-do.js";
import { containerLifecycleEvent, containerStoppedEvent } from "./lifecycle.js";
import { WebhookReplayQueue } from "./replay.js";
import { routeRequest } from "./router.js";
import { scheduledReassessment } from "./reassessment.js";

export { WebhookReplayQueue };
export { ProviderBudget };

export class PrAgentContainer extends Container {
  defaultPort = Number(runtime.PORT);
  sleepAfter = runtime.CONTAINER_SLEEP_AFTER;
  envVars = createPrAgentEnvironment(env);

  async interruptForRecoveryTest() {
    await this.destroy();
  }

  onStart() {
    console.log(containerLifecycleEvent("container started"));
  }

  onStop(params) {
    console.log(containerStoppedEvent(params));
  }

  // The container library sends SIGTERM when the idle timer expires. Recording
  // that is what separates an idle stop from a platform replacement.
  async onActivityExpired() {
    console.log(containerLifecycleEvent("container idle timer expired", { sleepAfter: this.sleepAfter }));
    await super.onActivityExpired();
  }

  onError(error) {
    console.error(containerLifecycleEvent("container error", { error: String(error) }));
    return error;
  }

}

export default { fetch: routeRequest, scheduled: scheduledReassessment };
