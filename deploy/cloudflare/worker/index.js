import { Container } from "@cloudflare/containers";
import { env } from "cloudflare:workers";

import { createPrAgentEnvironment } from "./configuration.js";
import { containerLifecycleEvent, containerStoppedEvent } from "./lifecycle.js";
import { WebhookReplayQueue } from "./replay.js";
import { routeRequest } from "./router.js";

export { WebhookReplayQueue };

export class PrAgentContainer extends Container {
  defaultPort = 3000;
  sleepAfter = "11m";
  envVars = createPrAgentEnvironment(env);

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

export default { fetch: routeRequest };
