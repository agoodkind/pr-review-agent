import { entryFromDelivery, forwardFailed } from "./replaylogic.js";
import { SERVICE_LOG_PATH, handleServiceLogs, verifyServiceLogSignature } from "./servicelogs.js";

// FORCE_REVIEW_LABEL_PREFIX names the labels that ask for a fresh full review.
// The Go service matches the same prefix on the delivery this worker forwards;
// what the worker adds is the restart, because only the worker owns the
// container.
const FORCE_REVIEW_LABEL_PREFIX = "test-review-agent-";

// forcesReview reports whether a delivery is one of those labels being added.
function forcesReview(metadata) {
  return (
    metadata.eventType === "pull_request" &&
    metadata.action === "labeled" &&
    metadata.label.startsWith(FORCE_REVIEW_LABEL_PREFIX)
  );
}

// restartOnForcedReview destroys the container when a signed delivery carries
// one of this service's labels, so the review that follows starts a new one.
//
// The signature is checked here rather than left to the Go service. Every other
// delivery is only forwarded, and the service verifies it before acting, but
// destroying the container is an action this worker takes before the service
// ever sees the body. Unverified, it is a denial of service anyone who can
// reach this worker can run: post a labeled payload, kill whatever review is
// in flight, and repeat. The verifier is the service log one because both use
// GitHub's own scheme and key.
async function restartOnForcedReview(container, env, request, body, metadata) {
  if (!forcesReview(metadata)) {
    return;
  }
  const signature = request.headers.get("x-hub-signature-256") ?? "";
  if (!(await verifyServiceLogSignature(env.GITHUB_WEBHOOK_SECRET, body, signature))) {
    // The delivery still goes to the Go service, which refuses it the way it
    // refuses any forged delivery. What it does not do is restart anything.
    console.error(JSON.stringify({ message: "container restart refused, invalid signature", ...metadata }));
    return;
  }
  // A restart that fails is logged and the delivery forwarded anyway. The
  // review is the point and the fresh environment is what the restart adds, so
  // refusing to review would cost more than reviewing on the old instance. The
  // log line is what tells an operator which one they got.
  try {
    await container.restartForForcedReview();
    console.log(JSON.stringify({ message: "container restarted for forced review", ...metadata }));
  } catch (error) {
    console.error(JSON.stringify({ message: "container restart failed", ...metadata, error: String(error) }));
  }
}

export async function routeRequest(request, env) {
  const url = new URL(request.url);
  if (request.method === "GET" && url.pathname === "/health") {
    return Response.json({ status: "ok" });
  }

  // Logs shipped by the container are printed here rather than forwarded on,
  // because the container is where they came from.
  if (url.pathname === SERVICE_LOG_PATH) {
    return handleServiceLogs(request, env.GITHUB_WEBHOOK_SECRET);
  }

  // The body is read up front because a delivery the container cannot take is
  // queued and replayed, and a consumed stream cannot be replayed. GitHub
  // bounds webhook payloads well under this worker's memory.
  const body = await request.clone().text();
  const metadata = await readWebhookMetadata(request);
  console.log(JSON.stringify({ message: "webhook forwarding", ...metadata }));

  let response = null;
  try {
    const container = env.PR_AGENT.getByName("github-app");
    await restartOnForcedReview(container, env, request, body, metadata);
    response = await container.fetch(request);
  } catch (error) {
    console.error(JSON.stringify({ message: "webhook forward threw", ...metadata, error: String(error) }));
  }

  // A 500 is the container library answering for a container that was not
  // there; the Go service never returns it. GitHub delivers a webhook once and
  // never redelivers a failure on its own, so dropping the delivery here left
  // pull requests with a required check that simply never appeared. The
  // delivery is queued and replayed instead, and GitHub is answered 202
  // because this worker now owns it.
  //
  // Only a delivery GitHub signed is queued. The signature is normally checked
  // by the Go service, which is exactly the part that is unavailable here, so
  // an unverified queue would let anyone fill the replay store with forged
  // bodies during an outage. The verifier is the service log one because both
  // use GitHub's own scheme and key.
  if (forwardFailed(response) && request.method === "POST" && metadata.deliveryId !== "") {
    const signature = request.headers.get("x-hub-signature-256") ?? "";
    if (!(await verifyServiceLogSignature(env.GITHUB_WEBHOOK_SECRET, body, signature))) {
      console.error(JSON.stringify({ message: "webhook replay refused, invalid signature", ...metadata }));
      return new Response("invalid signature", { status: 401 });
    }
    const queuedId = await enqueueForReplay(env, url.pathname, request, body, metadata);
    console.log(JSON.stringify({ message: "webhook queued for replay", ...metadata, queuedId }));
    return Response.json({ status: "queued for replay", deliveryId: queuedId }, { status: 202 });
  }
  if (response === null) {
    return Response.json({ status: "container unavailable" }, { status: 500 });
  }

  console.log(JSON.stringify({ message: "webhook forwarded", ...metadata, status: response.status }));
  return response;
}

async function enqueueForReplay(env, path, request, body, metadata) {
  const headers = {};
  for (const name of ["content-type", "x-github-event", "x-github-delivery", "x-hub-signature-256"]) {
    const value = request.headers.get(name);
    if (value !== null) {
      headers[name] = value;
    }
  }
  const entry = entryFromDelivery(metadata.deliveryId, path, headers, body, Date.now());
  const queue = env.REPLAY_QUEUE.getByName("webhook-replays");
  await queue.fetch(
    new Request("https://replay/enqueue", {
      method: "POST",
      body: JSON.stringify(entry),
    }),
  );
  return entry.id;
}

async function readWebhookMetadata(request) {
  const deliveryId = request.headers.get("x-github-delivery") ?? "";
  const eventType = request.headers.get("x-github-event") ?? "";
  if (eventType !== "pull_request") {
    return { deliveryId, eventType, action: "", head: "", label: "" };
  }

  try {
    const payload = await request.clone().json();
    return {
      deliveryId,
      eventType,
      action: payload.action ?? "",
      head: payload.pull_request?.head?.sha ?? "",
      // The label object is present only on a labeled or unlabeled delivery.
      // Every other action reads as an empty name, which matches no prefix.
      label: payload.label?.name ?? "",
    };
  } catch {
    return { deliveryId, eventType, action: "invalid_json", head: "", label: "" };
  }
}
