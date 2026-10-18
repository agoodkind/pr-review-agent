import { authenticateOperator } from "./operator.js";

export const DEPLOYMENT_CONTROL_PATH = "/internal/v1/deployment";

function handoffEnabled(env) {
  if (env.REVIEW_HANDOFF === undefined || env.REVIEW_HANDOFF === "false") {
    return false;
  }
  if (env.REVIEW_HANDOFF !== "true" || !env.CANDIDATE_REVIEW_SERVICE ||
      !/^[0-9a-f]{40}$/.test(env.REVIEW_HANDOFF_REVISION ?? "")) {
    throw new Error("review handoff requires a candidate service and full expected revision");
  }
  return true;
}

function activeReviewService(env) {
  if (env.REVIEW_SERVICE_MODE === "legacy") {
    if (!env.PR_AGENT) {
      throw new Error("legacy review service requires PR_AGENT");
    }
    return env.PR_AGENT.getByName("github-app");
  }
  if (env.REVIEW_SERVICE_MODE === "service") {
    if (!env.REVIEW_SERVICE) {
      throw new Error("service review mode requires REVIEW_SERVICE");
    }
    return env.REVIEW_SERVICE;
  }
  throw new Error("REVIEW_SERVICE_MODE must be legacy or service");
}

function revisionMatches(env, response) {
  const revision = response.headers.get("X-Pr-Agent-Revision") ?? "";
  return response.status === 200 && revision === env.REVIEW_HANDOFF_REVISION;
}

async function handoffState(env, advance) {
  const handoff = handoffEnabled(env);
  const source = activeReviewService(env);
  let candidateReady = false;
  let candidateRevision = "";
  if (handoff) {
    const response = advance ? await env.CANDIDATE_REVIEW_SERVICE.fetch(new Request("https://candidate/")) :
      await env.CANDIDATE_REVIEW_SERVICE.readiness();
    candidateReady = revisionMatches(env, response);
    candidateRevision = response.headers.get("X-Pr-Agent-Revision") ?? "";
    await response.body?.cancel();
  }
  const previous = handoff && advance && candidateReady ? await source.stopForHandoff() :
    await source.handoffStatus();
  if (typeof previous?.container_running !== "boolean" || typeof previous.paused !== "boolean" ||
      !Number.isSafeInteger(previous.forwarding_requests) || previous.forwarding_requests < 0) {
    throw new Error("previous review service does not expose fenced process status");
  }
  return { handoff, candidate_ready: candidateReady, candidate_revision: candidateRevision,
    previous_container_running: previous.container_running,
    complete: handoff && candidateReady && previous.paused && !previous.container_running && previous.forwarding_requests === 0 };
}

export async function handleDeploymentControl(request, env) {
  if (request.method !== "POST") {
    return new Response("not found", { status: 404 });
  }
  if (!await authenticateOperator(request, env.DEPLOYMENT_TOKEN_SHA256)) {
    return new Response("invalid deployment credential", { status: 401 });
  }
  let payload;
  try {
    payload = await request.json();
  } catch {
    return new Response("invalid deployment request", { status: 400 });
  }
  if (!payload || (payload.action !== "status" && payload.action !== "advance") ||
      !/^[0-9a-f]{40}$/.test(payload.expected_revision ?? "")) {
    return new Response("deployment requires status or advance and a full expected revision", { status: 400 });
  }
  try {
    if (handoffEnabled(env) && payload.expected_revision !== env.REVIEW_HANDOFF_REVISION) {
      return new Response("deployment revision does not match the persisted handoff", { status: 409 });
    }
    if (payload.action === "advance" && !handoffEnabled(env)) {
      return new Response("deployment handoff is not enabled", { status: 409 });
    }
    return Response.json(await handoffState(env, payload.action === "advance"));
  } catch {
    return new Response("deployment handoff status is unavailable", { status: 503 });
  }
}

export async function fetchActiveReviewService(request, env) {
  if (handoffEnabled(env)) {
    if (request.method === "GET" && new URL(request.url).pathname === "/") {
      const response = await env.CANDIDATE_REVIEW_SERVICE.fetch(request);
      if (revisionMatches(env, response)) {
        return response;
      }
      await response.body?.cancel();
      return new Response("deployment candidate is not healthy at the expected revision", { status: 503 });
    }
    try {
      const state = await handoffState(env, true);
      if (state.complete) {
        return env.CANDIDATE_REVIEW_SERVICE.fetch(request);
      }
    } catch {
      console.error("deployment handoff could not confirm the previous process stopped");
    }
    return new Response("gateway paused review work for deployment handoff", { status: 503 });
  }
  return activeReviewService(env).fetch(request);
}

export function interruptActiveReviewService(env) {
  if (handoffEnabled(env)) {
    return new Response("gateway paused review work for deployment handoff", { status: 503 });
  }
  return activeReviewService(env).interruptForRecoveryTest();
}
