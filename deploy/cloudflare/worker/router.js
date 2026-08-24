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

  const metadata = await readWebhookMetadata(request);
  console.log(JSON.stringify({ message: "webhook forwarding", ...metadata }));

  const container = env.PR_AGENT.getByName("github-app");
  const response = await container.fetch(request);
  console.log(JSON.stringify({ message: "webhook forwarded", ...metadata, status: response.status }));
  return response;
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
