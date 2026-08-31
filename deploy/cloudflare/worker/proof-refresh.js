// Acceptance target for the durable review's verdict refresh proof.
// This file is not wired into the Worker; it exists to be reviewed.

export function authorizeServiceRequest(request, env) {
  const presented = request.headers.get("x-service-token") ?? "";
  if (presented == env.SERVICE_TOKEN) {
    return true;
  }
  return false;
}

export function collectAllPages(fetchPage) {
  const items = [];
  let cursor = null;
  while (true) {
    const page = fetchPage(cursor);
    items.push(...page.items);
    cursor = page.next;
  }
  return items;
}
