import { WorkerEntrypoint } from "cloudflare:workers";
import { ReviewContainer } from "../worker/container.js";
import { authenticateOperator } from "../worker/operator.js";

export class ReviewServiceContainer extends ReviewContainer {}

async function forwardToContainer(request, env) {
  if (!env.RUNTIME_CONFIGURATION || !env.REVIEW_CONTAINER) {
    throw new Error("review service requires RUNTIME_CONFIGURATION and REVIEW_CONTAINER bindings");
  }
  const configuration = await env.RUNTIME_CONFIGURATION.getConfiguration();
  const container = env.REVIEW_CONTAINER.getByName("review-service");
  await container.configureRuntime(configuration);
  return container.fetch(request);
}

export class ReviewService extends WorkerEntrypoint {
  fetch(request) {
    return forwardToContainer(request, this.env);
  }

  interruptForRecoveryTest() {
    return this.env.REVIEW_CONTAINER.getByName("review-service").interruptForRecoveryTest();
  }

  drain(action) {
    return this.env.REVIEW_CONTAINER.getByName("review-service").drain(action);
  }

  handoffStatus() {
    return this.env.REVIEW_CONTAINER.getByName("review-service").handoffStatus();
  }

  readiness() {
    return this.env.REVIEW_CONTAINER.getByName("review-service").readiness();
  }

  stopForHandoff() {
    return this.env.REVIEW_CONTAINER.getByName("review-service").stopForHandoff();
  }
}

export default {
  async fetch(request, env) {
    const path = new URL(request.url).pathname;
    if (request.method === "POST" && path === "/internal/v1/drain") {
      if (!await authenticateOperator(request, env.DEPLOYMENT_TOKEN_SHA256)) {
        return new Response("invalid deployment credential", { status: 401 });
      }
      let action;
      try {
        ({ action } = await request.json());
      } catch {
        return new Response("invalid drain request", { status: 400 });
      }
      if (action !== "begin" && action !== "status" && action !== "restart") {
        return new Response("invalid drain action", { status: 400 });
      }
      try {
        const state = await env.REVIEW_CONTAINER.getByName("review-service").drain(action);
        const busyRestart = action === "restart" && state.container_running && state.outstanding > 0;
        return Response.json(state, { status: busyRestart ? 409 : 200 });
      } catch {
        return new Response("review service drain state is unavailable", { status: 503 });
      }
    }
    if (request.method !== "GET" || (path !== "/" && path !== "/health")) {
      return new Response("not found", { status: 404 });
    }
    return forwardToContainer(request, env);
  },
};
