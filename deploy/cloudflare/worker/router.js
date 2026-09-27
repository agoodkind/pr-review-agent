import {
  REVIEW_SETTINGS_HEADER,
  REVIEW_SETTINGS_SIGNATURE_HEADER,
  createReviewSettingsHeader,
  signReviewSettings,
} from "./configuration.js";
import { deliverySettled, entryFromDelivery, forwardFailed } from "./replaylogic.js";
import { SERVICE_LOG_PATH, handleServiceLogs, verifyServiceLogSignature } from "./servicelogs.js";

export async function routeRequest(request, env) {
  const url = new URL(request.url);
  if (request.method === "GET" && url.pathname === "/health") {
    return Response.json({ status: "ok" });
  }
  if (url.pathname === SERVICE_LOG_PATH) {
    return handleServiceLogs(request, env.GITHUB_WEBHOOK_SECRET);
  }

  const body = await request.clone().text();
  const metadata = await readWebhookMetadata(request);
  console.log(JSON.stringify({ message: "webhook forwarding", ...metadata }));
  const forwarded = await withReviewSettings(request, body, env);

  let replayQueue = null;
  if (request.method === "POST" && url.pathname === "/api/v1/github_webhooks" && metadata.deliveryId !== "") {
    const signature = request.headers.get("x-hub-signature-256") ?? "";
    if (!(await verifyServiceLogSignature(env.GITHUB_WEBHOOK_SECRET, body, signature))) {
      return new Response("invalid signature", { status: 401 });
    }
    replayQueue = env.REPLAY_QUEUE.getByName("webhook-replays");
    try {
      const accepted = await enqueueForReplay(replayQueue, url.pathname, forwarded, body, metadata);
      if (!accepted) {
        return new Response("replay queue unavailable", { status: 503 });
      }
    } catch (error) {
      console.error(JSON.stringify({ message: "webhook queue unavailable", ...metadata, error: String(error) }));
      return new Response("replay queue unavailable", { status: 503 });
    }
  }

  let response = null;
  try {
    const container = env.PR_AGENT.getByName("github-app");
    response = await container.fetch(forwarded);
  } catch (error) {
    console.error(JSON.stringify({ message: "webhook forward threw", ...metadata, error: String(error) }));
  }

  if (replayQueue !== null && deliverySettled(response)) {
    await settleReplay(replayQueue, metadata.deliveryId);
  }
  if (replayQueue !== null && forwardFailed(response)) {
    return Response.json({ status: "queued for replay", deliveryId: metadata.deliveryId }, { status: 202 });
  }
  if (response === null) {
    return Response.json({ status: "container unavailable" }, { status: 500 });
  }

  console.log(JSON.stringify({ message: "webhook forwarded", ...metadata, status: response.status }));
  return response;
}

async function enqueueForReplay(queue, path, request, body, metadata) {
  const headers = {};
  for (const name of [
    "content-type",
    "x-github-event",
    "x-github-delivery",
    "x-hub-signature-256",
    REVIEW_SETTINGS_HEADER,
    REVIEW_SETTINGS_SIGNATURE_HEADER,
  ]) {
    const value = request.headers.get(name);
    if (value !== null) {
      headers[name] = value;
    }
  }
  const entry = entryFromDelivery(metadata.deliveryId, path, headers, body, Date.now());
  const response = await queue.fetch(new Request("https://replay/enqueue", {
    method: "POST",
    body: JSON.stringify(entry),
  }));
  return response.ok;
}

async function settleReplay(queue, deliveryId) {
  try {
    const response = await queue.fetch(new Request("https://replay/settle", {
      method: "POST",
      body: JSON.stringify({ id: deliveryId }),
    }));
    if (!response.ok) {
      console.error(JSON.stringify({ message: "webhook settle failed", deliveryId, status: response.status }));
    }
  } catch (error) {
    console.error(JSON.stringify({ message: "webhook settle failed", deliveryId, error: String(error) }));
  }
}

async function withReviewSettings(request, body, env) {
  const forwarded = new Request(request);
  forwarded.headers.delete(REVIEW_SETTINGS_HEADER);
  forwarded.headers.delete(REVIEW_SETTINGS_SIGNATURE_HEADER);

  const settings = createReviewSettingsHeader();
  if (settings === "" || !env.GITHUB_WEBHOOK_SECRET) {
    return forwarded;
  }
  forwarded.headers.set(REVIEW_SETTINGS_HEADER, settings);
  forwarded.headers.set(
    REVIEW_SETTINGS_SIGNATURE_HEADER,
    await signReviewSettings(env.GITHUB_WEBHOOK_SECRET, settings, body),
  );
  return forwarded;
}

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
      label: stringField(payload.label?.name),
    };
  } catch {
    return { deliveryId, eventType, action: "invalid_json", head: "", label: "" };
  }
}
