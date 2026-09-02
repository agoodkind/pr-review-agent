import { REVIEW_SETTINGS_HEADER, createReviewSettingsHeader } from "./configuration.js";
import { entryFromDelivery, forwardFailed } from "./replaylogic.js";
import { SERVICE_LOG_PATH, handleServiceLogs, verifyServiceLogSignature } from "./servicelogs.js";

// This worker forwards every delivery and decides nothing about any of them.
//
// It used to destroy the container when a delivery carried a forcing label, so
// the review that followed read its configuration fresh. Nothing here does that
// any more: a restart takes down whatever reviews are in flight, and the label
// asks for a full review rather than for other people's work to be killed. The
// configuration it was reaching for travels with the delivery instead.
//
// Losing the restart takes the label inspection with it, and the signature check
// that existed only to gate it. The Go service already verifies every delivery
// before acting and already applies the label and draft rules, so nothing this
// file dropped went unenforced; it stopped being enforced twice.
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

  // The tuning values ride on the forwarded request rather than on the container
  // environment, because a running container keeps whatever it booted with: a
  // chunk timeout changed and restored still governed reviews long afterwards,
  // because nothing had replaced the process. Attaching them per delivery is what
  // makes a correction take effect on the next review.
  //
  // The header is set on a copy rather than on the caller's request, so the body
  // GitHub signed and the entry queued for replay stay exactly as they arrived.
  const forwarded = withReviewSettings(request, env);

  let response = null;
  try {
    const container = env.PR_AGENT.getByName("github-app");
    response = await container.fetch(forwarded);
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

// withReviewSettings returns the request to forward, carrying this worker's
// review tuning values.
//
// A worker with none configured forwards the request untouched, and the service
// then runs on what it booted with. Any header the caller sent under this name
// is replaced rather than added to, so a value can only come from this worker's
// own bindings.
function withReviewSettings(request, env) {
  const settings = createReviewSettingsHeader(env);
  if (settings === "") {
    return request;
  }
  const forwarded = new Request(request);
  forwarded.headers.set(REVIEW_SETTINGS_HEADER, settings);
  return forwarded;
}

// stringField returns a payload value when it is a string and the empty string
// otherwise, so every metadata field is one whatever the sender wrote.
//
// The metadata is built from a body nobody has verified and goes straight into a
// log line, and a log line must not be able to throw on what a stranger put in
// the payload. It once did: a reader called a string method on the label, and a
// labeled payload carrying a number for its name threw before the delivery was
// forwarded. That reader is gone, and this stays, because the next one should
// not have to rediscover that these values are whatever the sender wrote.
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
    return { deliveryId, eventType, action: "", head: "", label: "" };
  }

  try {
    const payload = await request.clone().json();
    return {
      deliveryId,
      eventType,
      action: stringField(payload.action),
      head: stringField(payload.pull_request?.head?.sha),
      // The label object is present only on a labeled or unlabeled delivery.
      // Every other action reads as an empty name, and so does a name of any
      // type but a string. Nothing acts on it: it is here so an operator reading
      // the log can tie a run back to the label somebody added.
      label: stringField(payload.label?.name),
    };
  } catch {
    return { deliveryId, eventType, action: "invalid_json", head: "", label: "" };
  }
}
