import { entryFromDelivery, forwardFailed } from "./replaylogic.js";
import { SERVICE_LOG_PATH, handleServiceLogs } from "./servicelogs.js";

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
  if (forwardFailed(response) && request.method === "POST" && metadata.deliveryId !== "") {
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
    return { deliveryId, eventType, action: "", head: "" };
  }

  try {
    const payload = await request.clone().json();
    return {
      deliveryId,
      eventType,
      action: payload.action ?? "",
      head: payload.pull_request?.head?.sha ?? "",
    };
  } catch {
    return { deliveryId, eventType, action: "invalid_json", head: "" };
  }
}
