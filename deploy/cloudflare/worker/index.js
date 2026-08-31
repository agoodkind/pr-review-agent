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

  // A forced review must run on a fresh container. The container reads its
  // environment once, at start, so a running instance keeps whatever values it
  // booted with: a chunk timeout changed to 6 seconds and restored 5 minutes
  // later still governed a real pull request 13 minutes after the restore,
  // because nothing had restarted the container in between. Reviewing again on
  // the same instance would prove nothing about the current configuration.
  //
  // destroy is the container library's own termination path, and it resolves
  // once the instance is gone, so the delivery forwarded next starts a new one.
  // It is a SIGKILL, which is why only an explicit label triggers it.
  async restartForForcedReview() {
    console.log(containerLifecycleEvent("container restart requested"));
    await this.destroy();
  }
}

export default { fetch: routeRequest };
