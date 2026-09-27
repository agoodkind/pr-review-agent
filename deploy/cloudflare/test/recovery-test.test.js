import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import test from "node:test";

import { recoveryTest } from "../worker/recovery-test.js";
import { routeRequest } from "../worker/router.js";

const webhookSecret = "recovery-test-secret"; // gitleaks:allow

function webhookRequest(payload, validSignature = true) {
  const body = JSON.stringify(payload);
  const signature = createHmac("sha256", webhookSecret).update(body).digest("hex");
  return new Request("https://reviewer.example/api/v1/github_webhooks", {
    method: "POST",
    body,
    headers: {
      "content-type": "application/json",
      "x-github-event": "pull_request",
      "x-github-delivery": "delivery-recovery-test",
      "x-hub-signature-256": `sha256=${validSignature ? signature : "0".repeat(64)}`,
    },
  });
}

function interruptPayload() {
  return {
    action: "labeled",
    repository: { full_name: recoveryTest.repository },
    pull_request: {
      number: recoveryTest.pullRequest,
      labels: [{ name: recoveryTest.forceLabel }, { name: recoveryTest.interruptLabel }],
    },
    label: { name: recoveryTest.interruptLabel },
  };
}

function environment(events) {
  return {
    GITHUB_WEBHOOK_SECRET: webhookSecret, // gitleaks:allow
    PR_AGENT: {
      getByName() {
        return {
          async interruptForRecoveryTest() {
            events.push("interrupt");
          },
          async fetch() {
            events.push("forward");
            return new Response("accepted", { status: 202 });
          },
        };
      },
    },
    REPLAY_QUEUE: {
      getByName() {
        return {
          async fetch() {
            events.push("enqueue");
            return Response.json({ queued: true });
          },
        };
      },
    },
  };
}

test("a signed label on the configured test pull request interrupts the container", async function () {
  const events = [];
  const response = await routeRequest(webhookRequest(interruptPayload()), environment(events));

  assert.equal(response.status, 200);
  assert.deepEqual(events, ["interrupt"]);
});

test("other deliveries cannot interrupt the container", async function () {
  const cases = [
    { label: { name: "other-label" } },
    { repository: { full_name: "agoodkind/other" } },
    { pull_request: { ...interruptPayload().pull_request, number: 325 } },
    { pull_request: { ...interruptPayload().pull_request, labels: [] } },
    { action: "unlabeled" },
  ];

  for (const changes of cases) {
    const events = [];
    const payload = { ...interruptPayload(), ...changes };
    const response = await routeRequest(webhookRequest(payload), environment(events));
    assert.equal(response.status, 202);
    assert.deepEqual(events, ["enqueue", "forward"]);
  }

  const events = [];
  const response = await routeRequest(webhookRequest(interruptPayload(), false), environment(events));
  assert.equal(response.status, 401);
  assert.deepEqual(events, []);
});
