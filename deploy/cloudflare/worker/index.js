import { Container } from "@cloudflare/containers";
import { env } from "cloudflare:workers";

import { createPrAgentEnvironment } from "./configuration.js";
import { routeRequest } from "./router.js";

export class PrAgentContainer extends Container {
  defaultPort = 3000;
  sleepAfter = "11m";
  envVars = createPrAgentEnvironment(env);
}

export default { fetch: routeRequest };
