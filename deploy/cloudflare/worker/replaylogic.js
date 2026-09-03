// Pure decisions for the webhook replay queue, kept free of Cloudflare
// imports so node can test them directly.

// CONTAINER_ABSENT_STATUS is the one status the Go service never returns, so
// seeing it means the container library answered for a container that was not
// there.
const CONTAINER_ABSENT_STATUS = 500;

// QUEUE_FULL_STATUS is the service saying it accepted nothing because its own
// review queue was full. It is the one status the service returns that must be
// replayed rather than passed through: the delivery was admitted on GitHub and
// then dropped, so a check exists that nothing will ever finish, and GitHub
// does not redeliver on its own. Every other status the service returns is an
// answer about the delivery rather than a refusal to take it.
const QUEUE_FULL_STATUS = 503;

// abandonAfterMs bounds how long a delivery is retried. A day covers any
// realistic outage; past it the entry is dropped loudly rather than replayed
// into a pull request nobody remembers.
export const abandonAfterMs = 24 * 60 * 60 * 1000;

// firstDelayMs is the first retry delay. A crash looping container needs
// minutes, not milliseconds, but the common cold start recovers in seconds.
export const firstDelayMs = 10_000;

// maxDelayMs caps the backoff so a long outage is probed every few minutes.
export const maxDelayMs = 5 * 60 * 1000;

// forwardFailed reports whether a forward outcome means the container never
// processed the delivery. A thrown fetch has no response at all.
export function forwardFailed(response) {
  if (response === null) {
    return true;
  }
  return response.status === CONTAINER_ABSENT_STATUS || response.status === QUEUE_FULL_STATUS;
}

// replayDelayMs returns the backoff before the next attempt.
export function replayDelayMs(attempts) {
  const delay = firstDelayMs * 2 ** attempts;
  return Math.min(delay, maxDelayMs);
}

// shouldAbandon reports whether an entry has been owed longer than the window.
export function shouldAbandon(entry, now) {
  return now - entry.firstSeen >= abandonAfterMs;
}

// dueEntries returns the entries whose backoff has elapsed.
export function dueEntries(entries, now) {
  return entries.filter(function isDue(entry) {
    return entry.notBefore <= now;
  });
}

// nextWakeAt returns the earliest moment any entry becomes due, or null when
// the queue is empty and no alarm is needed.
export function nextWakeAt(entries) {
  let earliest = null;
  for (const entry of entries) {
    if (earliest === null || entry.notBefore < earliest) {
      earliest = entry.notBefore;
    }
  }
  return earliest;
}

// entryFromDelivery captures the parts of a webhook delivery a replay needs.
// The signature header must survive verbatim, because the Go service verifies
// the body against it exactly as GitHub signed it.
export function entryFromDelivery(deliveryId, path, headers, body, now) {
  return {
    id: deliveryId === "" ? crypto.randomUUID() : deliveryId,
    path,
    headers,
    body,
    attempts: 0,
    notBefore: now + firstDelayMs,
    firstSeen: now,
  };
}
