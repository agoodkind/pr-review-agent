# Review handoff during container deployment

## Failure model

The Worker can accept a webhook while its container is about to stop. A container replacement sends SIGTERM to the old process and sends SIGKILL after 15 minutes if that process has not exited. A completed HTTP admission does not prove that the background review finished. Without a durable record, an interrupted review can leave an in-progress GitHub check indefinitely.

Cloudflare starts the replacement for a named container after the old instance exits. The Worker deployment can become active before the container image changes. The old and new Worker versions must tolerate different service versions during a rollout. Cloudflare documents the container sequence in its [rollout guide](https://developers.cloudflare.com/containers/configuration/rollouts/).

## Durable admission

The Worker verifies the GitHub signature and stores the signed request body, path, and replay headers in a Durable Object before forwarding the request. The delivery identifier is the storage key. Repeated admission with that identifier preserves the original record. A storage failure returns HTTP 503 and prevents forwarding. The Worker returns HTTP 202 after a container transport failure because the Durable Object has accepted the request.

The Go service returns `X-Pr-Agent-Delivery-State: pending` when the review is queued or running. It returns `settled` after a terminal result or when the event requires no review. The Worker deletes the durable record only for `settled` or a terminal client error. The Worker keeps a record when the response omits the header. An older service version omits the header during a rollout.

## Retry and shutdown

The Durable Object alarm retries pending deliveries. The first delay is 10 seconds. Exponential backoff stops increasing at five minutes. A review that takes longer than one retry interval can receive a duplicate admission. The service checks its delivery state and does not start a second review. The durable record remains after 24 hours and produces an overdue log once. A temporary outage cannot silently erase an accepted review.

SIGTERM stops new HTTP admissions. The Go dispatcher waits up to 14 minutes for active reviews to finish. A review that finishes during the drain settles its durable delivery. A review that exceeds the drain remains pending when the worker context is canceled. After the old container exits, the next alarm forwards that record to the replacement. The replacement checks GitHub's persisted review state before deciding whether to resume or settle the delivery. The five-minute retry cap bounds the recovery delay after container exit. During the drain period, retries can confirm pending status against the old process without starting a second review.

## Release order

The release deploys a Worker version before replacing the image for its named container. The Worker retains headerless responses from an older container. Verify the Worker version and immutable image digest after release. A green release job does not prove that an interrupted review resumed. Inspect a completed `PR-Agent Review` check for a delivery admitted during a controlled container restart.
