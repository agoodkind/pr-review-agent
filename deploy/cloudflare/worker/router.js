import { entryFromDelivery, forwardFailed } from "./replaylogic.js";
import { SERVICE_LOG_PATH, handleServiceLogs, verifyServiceLogSignature } from "./servicelogs.js";

// FORCE_REVIEW_LABEL_PREFIX names the labels that ask for a fresh full review.
// The Go service matches the same prefix on the delivery this worker forwards;
// what the worker adds is the restart, because only the worker owns the
// container.
const FORCE_REVIEW_LABEL_PREFIX = "test-review-agent-";

// forcesReview reports whether a delivery is one of those labels being added.
//
// It reads metadata taken from a body nobody has verified, and it runs before
// the signature is checked, so it must not throw whatever that body contains.
// What makes the prefix test safe is that readWebhookMetadata narrows every
// field to a string, so a label of any other type arrives as no label and the
// delivery is forwarded for the Go service to judge.
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
  // A worker with no signing key can verify nothing, so it restarts nothing, but
  // that is a misconfiguration rather than a forged delivery and it reads
  // nothing like one from the outside: every forced label is quietly ignored
  // while the signature on it was perfectly good. Saying so is what stops an
  // operator hunting a forgery that never happened.
  if (!env.GITHUB_WEBHOOK_SECRET) {
    console.error(JSON.stringify({ message: "container restart refused, no signing key configured", ...metadata }));
    return;
  }
  const signature = request.headers.get("x-hub-signature-256") ?? "";
  if (!(await verifyServiceLogSignature(env.GITHUB_WEBHOOK_SECRET, body, signature))) {
    // The delivery still goes to the Go service, which refuses it the way it
    // refuses any forged delivery. What it does not do is restart anything.
    console.error(JSON.stringify({ message: "container restart refused, invalid signature", ...metadata }));
    return;
  }
  // A draft is never reviewed, and a label does not change that. The Go service
  // refuses a labeled delivery on a draft outright, so restarting for one would
  // destroy whatever review is in flight and produce no review in its place.
  // Anyone who can add a label could repeat that at will, which makes the two
  // rules agreeing a requirement rather than a tidiness.
  //
  // The check sits after the signature so a forged delivery still reports the
  // forgery, which is the more important line for a reader, rather than being
  // filed under the draft it also claimed to be.
  if (metadata.draft) {
    console.log(JSON.stringify({ message: "container restart skipped, draft pull request", ...metadata }));
    return;
  }
  // A restart that fails takes the delivery down with it, and the caller queues
  // it for replay the way it queues any delivery the container could not take.
  //
  // This reverses the earlier rule, which logged the failure and forwarded
  // anyway on the reasoning that a review on the old instance beats no review.
  // The fresh container is not a bonus on top of the review, it is the thing the
  // label asks for: the container reads its environment once at start, so a
  // review on the instance the restart failed to replace answers for the
  // configuration somebody was trying to get rid of, and answers with the
  // authority of a completed check. Failing here sends the delivery down the
  // signed replay path instead, where it is retried rather than lost.
  await container.restartForForcedReview();
  console.log(JSON.stringify({ message: "container restarted for forced review", ...metadata }));
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

// stringField returns a payload value when it is a string and the empty string
// otherwise, so every metadata field holds its own type whatever the sender
// wrote.
//
// The narrowing is the point rather than a nicety. This metadata is built from a
// body nobody has verified, it is read before the signature is checked, and one
// reader calls a string method on it. Leaving the raw types in place let a
// labeled payload carrying a number for its name throw there, and the throw was
// indistinguishable from a container that could not take the delivery: the
// forward was abandoned, and a signed delivery went to the replay queue to fail
// the same way on every attempt.
function stringField(value) {
  if (typeof value === "string") {
    return value;
  }
  return "";
}

async function readWebhookMetadata(request) {
  const deliveryId = request.headers.get("x-github-delivery") ?? "";
  const eventType = request.headers.get("x-github-event") ?? "";
  if (eventType !== "pull_request") {
    return { deliveryId, eventType, action: "", head: "", label: "", draft: false };
  }

  try {
    const payload = await request.clone().json();
    return {
      deliveryId,
      eventType,
      action: stringField(payload.action),
      head: stringField(payload.pull_request?.head?.sha),
      // The label object is present only on a labeled or unlabeled delivery.
      // Every other action reads as an empty name, which matches no prefix, and
      // so does a name of any type but a string.
      label: stringField(payload.label?.name),
      // Narrowed the same way the string fields are, and to false rather than
      // true when the payload says something else: a delivery whose draft flag
      // cannot be read is treated as a pull request the service would review,
      // which is what the Go decoder does with a missing or malformed flag.
      draft: payload.pull_request?.draft === true,
    };
  } catch {
    return { deliveryId, eventType, action: "invalid_json", head: "", label: "", draft: false };
  }
}
