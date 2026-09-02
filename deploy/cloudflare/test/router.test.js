import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { createHmac } from "node:crypto";
import { once } from "node:events";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";

let routeRequest;
let createPrAgentEnvironment;
const execFileAsync = promisify(execFile);

try {
  ({ routeRequest } = await import("../worker/router.js"));
} catch {}

try {
  ({ createPrAgentEnvironment } = await import("../worker/configuration.js"));
} catch {}

function createEnvironment(forwardedRequests) {
  return {
    PR_AGENT: {
      getByName(name) {
        assert.equal(name, "github-app");
        return {
          async fetch(request) {
            forwardedRequests.push(request);
            return new Response("proxied", { status: 202 });
          },
        };
      },
    },
  };
}

test("health does not start the review container", async function () {
  assert.equal(typeof routeRequest, "function");

  const forwardedRequests = [];
  const response = await routeRequest(
    new Request("https://reviewer.example/health"),
    createEnvironment(forwardedRequests),
  );

  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), { status: "ok" });
  assert.equal(forwardedRequests.length, 0);
});

test("webhooks reach the Go review container", async function () {
  assert.equal(typeof routeRequest, "function");

  const forwardedRequests = [];
  const request = new Request("https://reviewer.example/api/v1/github_webhooks", {
    body: "{}",
    method: "POST",
  });

  const response = await routeRequest(request, createEnvironment(forwardedRequests));

  assert.equal(response.status, 202);
  assert.equal(forwardedRequests.length, 1);
});

// GitHub delivers a webhook once and never redelivers a failure on its own.
// One live container outage answered 500 to 33 deliveries, and each affected
// pull request was left with a required check that simply never appeared,
// blocked with nothing a person could point at. A delivery the container
// cannot take is queued and GitHub is answered 202, because the worker now
// owns it.
const testWebhookSecret = "test-webhook-secret"; // gitleaks:allow

function signBody(body) {
  return "sha256=" + createHmac("sha256", testWebhookSecret).update(body).digest("hex"); // gitleaks:allow
}

function createFailingEnvironment(mode, queued) {
  return {
    GITHUB_WEBHOOK_SECRET: testWebhookSecret, // gitleaks:allow
    PR_AGENT: {
      getByName() {
        return {
          async fetch() {
            if (mode === "throw") {
              throw new Error("There is no container instance that can be provided");
            }
            return new Response("no container", { status: 500 });
          },
        };
      },
    },
    REPLAY_QUEUE: {
      getByName(name) {
        assert.equal(name, "webhook-replays");
        return {
          async fetch(request) {
            queued.push(await request.json());
            return Response.json({ queued: true });
          },
        };
      },
    },
  };
}

function signedWebhookRequest(signature) {
  const body = JSON.stringify({ action: "opened", pull_request: { head: { sha: "e6949cd" } } });
  return new Request("https://reviewer.example/api/v1/github_webhooks", {
    body,
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-github-event": "pull_request",
      "x-github-delivery": "delivery-lost-1",
      "x-hub-signature-256": signature ?? signBody(body),
    },
  });
}

test("a delivery the container fetch throws on is queued and GitHub answered 202", async function () {
  const queued = [];
  const response = await routeRequest(signedWebhookRequest(), createFailingEnvironment("throw", queued));

  assert.equal(response.status, 202);
  assert.equal(queued.length, 1);
  assert.equal(queued[0].id, "delivery-lost-1");
  assert.equal(queued[0].path, "/api/v1/github_webhooks");
  assert.match(queued[0].headers["x-hub-signature-256"], /^sha256=/);
  assert.match(queued[0].body, /opened/);
});

// The signature is normally checked by the Go service, which is exactly the
// part that is unavailable when the queue is in play. An unverified queue
// would let anyone fill the replay store with forged bodies during an outage.
test("a forged delivery is refused during an outage and queues nothing", async function () {
  const queued = [];
  const response = await routeRequest(
    signedWebhookRequest("sha256=" + "0".repeat(64)),
    createFailingEnvironment("throw", queued),
  );

  assert.equal(response.status, 401);
  assert.equal(queued.length, 0);
});

test("a delivery answered 500 is queued, because the Go service never returns 500", async function () {
  const queued = [];
  const response = await routeRequest(signedWebhookRequest(), createFailingEnvironment("status500", queued));

  assert.equal(response.status, 202);
  assert.equal(queued.length, 1);
});

test("a service answer that is not 500 passes through untouched and queues nothing", async function () {
  const queued = [];
  const environment = createFailingEnvironment("throw", queued);
  environment.PR_AGENT = {
    getByName() {
      return {
        async fetch() {
          return new Response("invalid signature", { status: 401 });
        },
      };
    },
  };

  const response = await routeRequest(signedWebhookRequest(), environment);

  assert.equal(response.status, 401);
  assert.equal(queued.length, 0);
});

// The container reads its environment once, at start. A chunk timeout changed
// to 6 seconds and restored 5 minutes later still governed a real pull request
// 13 minutes after the restore, because nothing had restarted the container.
// A forced review therefore has to reach a fresh container, and the worker is
// the only part that owns one.
function createRestartEnvironment(events, queued) {
  return {
    GITHUB_WEBHOOK_SECRET: testWebhookSecret, // gitleaks:allow
    PR_AGENT: {
      getByName(name) {
        assert.equal(name, "github-app");
        return {
          async restartForForcedReview() {
            events.push("restart");
          },
          async fetch() {
            events.push("forward");
            return new Response("proxied", { status: 202 });
          },
        };
      },
    },
    // A queue is bound so a test can prove a delivery did not reach it. Without
    // one, a delivery that was queued would fail on the missing binding and read
    // as some other fault.
    REPLAY_QUEUE: {
      getByName(name) {
        assert.equal(name, "webhook-replays");
        return {
          async fetch(request) {
            (queued ?? []).push(await request.json());
            return Response.json({ queued: true });
          },
        };
      },
    },
  };
}

function labeledWebhookRequest(action, labelName, signature, draft) {
  const body = JSON.stringify({
    action,
    label: { name: labelName },
    pull_request: { draft: draft === true, head: { sha: "e6949cd" } },
  });
  return new Request("https://reviewer.example/api/v1/github_webhooks", {
    body,
    method: "POST",
    headers: {
      "content-type": "application/json",
      "x-github-event": "pull_request",
      "x-github-delivery": "delivery-label-1",
      "x-hub-signature-256": signature ?? signBody(body),
    },
  });
}

test("a label this service owns stops the container before the delivery is forwarded", async function () {
  const events = [];

  const response = await routeRequest(
    labeledWebhookRequest("labeled", "test-review-agent-rerun"),
    createRestartEnvironment(events),
  );

  assert.equal(response.status, 202);
  assert.deepEqual(events, ["restart", "forward"]);
});

// Destroying the container is the one action this worker takes before the Go
// service ever sees the body. Unverified it is a denial of service anyone who
// can reach this worker can run: post a labeled payload, kill the review in
// flight, repeat. The delivery is still forwarded, because the Go service is
// what refuses a forged signature; what must not happen is the restart.
test("a forged forcing delivery restarts nothing", async function () {
  for (const signature of ["sha256=" + "0".repeat(64), "", "not-a-signature"]) {
    const events = [];
    const response = await routeRequest(
      labeledWebhookRequest("labeled", "test-review-agent-rerun", signature),
      createRestartEnvironment(events),
    );

    assert.equal(response.status, 202);
    assert.deepEqual(events, ["forward"], `signature ${JSON.stringify(signature)} restarted the container`);
  }
});

// A restart that fails must not let the forced review proceed. The container
// reads its environment once at start, so reviewing on the instance the restart
// could not replace answers for the configuration the label was added to get rid
// of, and answers with the authority of a completed check.
//
// The failure travels out to the forwarding handler, which queues the delivery
// the way it queues any the container could not take, so the request is retried
// rather than lost. Only a signed delivery reaches the restart, so it is also
// one the replay path accepts.
test("a forced review whose restart fails is queued rather than run on the old container", async function () {
  const events = [];
  const queued = [];
  const environment = createRestartEnvironment(events, queued);
  environment.PR_AGENT = {
    getByName() {
      return {
        async restartForForcedReview() {
          throw new Error("destroy failed");
        },
        async fetch() {
          events.push("forward");
          return new Response("proxied", { status: 202 });
        },
      };
    },
  };

  const response = await routeRequest(labeledWebhookRequest("labeled", "test-review-agent-rerun"), environment);

  assert.equal(response.status, 202);
  assert.deepEqual(events, [], "the review ran on the container the restart failed to replace");
  assert.equal(queued.length, 1, "the forced delivery was dropped rather than queued for replay");
  assert.equal(queued[0].id, "delivery-label-1");
});

// A worker with no signing key configured can verify nothing, so it must
// restart nothing rather than treat an unverifiable delivery as trusted.
//
// It must also say which of the two it is. A missing key and a forged signature
// both stop the restart, and reporting the misconfiguration as a bad signature
// sends whoever is asking why forced restarts never happen looking for a forgery
// that never happened, while the signature on every one of those deliveries was
// good.
test("a forcing delivery restarts nothing when no signing key is configured", async function () {
  const events = [];
  const environment = createRestartEnvironment(events);
  delete environment.GITHUB_WEBHOOK_SECRET;
  const logged = [];
  const realError = console.error;
  console.error = function (line) {
    logged.push(line);
  };

  let response;
  try {
    response = await routeRequest(labeledWebhookRequest("labeled", "test-review-agent-rerun"), environment);
  } finally {
    console.error = realError;
  }

  const refusals = logged.filter(function (line) {
    return line.includes("container restart refused");
  });
  assert.equal(refusals.length, 1, `refusal lines = ${JSON.stringify(logged)}`);
  assert.match(refusals[0], /no signing key configured/);
  assert.doesNotMatch(refusals[0], /invalid signature/);

  assert.equal(response.status, 202);
  assert.deepEqual(events, ["forward"]);
});

// The Go service refuses a labeled delivery on a draft outright, because a
// draft is never reviewed and a label does not change that. A worker that
// restarted anyway would destroy whatever review is in flight and produce no
// review in its place, and anyone who can add a label could repeat that at will.
//
// The forcing label is the same one that restarts a non-draft pull request, so
// the draft flag is the only thing separating this case from that one.
test("a forcing label on a draft pull request restarts nothing", async function () {
  const events = [];
  const queued = [];

  const response = await routeRequest(
    labeledWebhookRequest("labeled", "test-review-agent-rerun", undefined, true),
    createRestartEnvironment(events, queued),
  );

  assert.equal(response.status, 202);
  assert.deepEqual(events, ["forward"]);
  assert.equal(queued.length, 0);
});

// A draft flag the payload does not carry, or carries as something other than
// true, must not suppress the restart. Reading any of those as a draft would
// silently disable the label on ordinary pull requests.
test("a forcing label restarts when the draft flag is absent or not true", async function () {
  for (const draft of [undefined, false, "true", 0, null]) {
    const events = [];
    const body = JSON.stringify({
      action: "labeled",
      label: { name: "test-review-agent-rerun" },
      pull_request: { draft, head: { sha: "e6949cd" } },
    });
    const request = new Request("https://reviewer.example/api/v1/github_webhooks", {
      body,
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-github-event": "pull_request",
        "x-github-delivery": "delivery-label-1",
        "x-hub-signature-256": signBody(body),
      },
    });

    const response = await routeRequest(request, createRestartEnvironment(events, []));

    assert.equal(response.status, 202, `draft ${JSON.stringify(draft)}`);
    assert.deepEqual(events, ["restart", "forward"], `draft ${JSON.stringify(draft)}`);
  }
});

// The label name comes out of a body nobody has verified, and the forcing check
// reads it before the signature is checked, so it has to survive whatever the
// sender wrote. A name of any type but a string is no label: the delivery is
// forwarded for the Go service to judge, exactly as every other delivery is.
//
// Before this the prefix test called a string method on it and threw, in the one
// place this worker cannot afford to. The delivery then never reached the
// container at all: an unsigned one was answered 401 and a signed one was
// diverted into the replay queue, where every attempt would throw the same way.
test("a label name that is not a string is no label rather than an error", async function () {
  const forgedSignature = "sha256=" + "0".repeat(64);
  for (const labelName of [12345, 3.14, true, { nested: "object" }, ["array"]]) {
    for (const signed of [true, false]) {
      const events = [];
      const queued = [];
      const request = signed
        ? labeledWebhookRequest("labeled", labelName)
        : labeledWebhookRequest("labeled", labelName, forgedSignature);
      const where = `label ${JSON.stringify(labelName)} signed=${signed}`;

      const response = await routeRequest(request, createRestartEnvironment(events, queued));

      assert.equal(response.status, 202, where);
      assert.deepEqual(events, ["forward"], where);
      assert.equal(queued.length, 0, where);
    }
  }
});

test("no other delivery stops the container", async function () {
  const cases = [
    ["labeled", "needs-review"],
    ["labeled", "review-agent-test"],
    ["unlabeled", "test-review-agent-rerun"],
    ["synchronize", ""],
    ["opened", ""],
  ];

  for (const [action, labelName] of cases) {
    const events = [];
    const response = await routeRequest(
      labeledWebhookRequest(action, labelName),
      createRestartEnvironment(events),
    );

    assert.equal(response.status, 202);
    assert.deepEqual(events, ["forward"], `${action} ${labelName} stopped the container`);
  }
});

test("health probe exits when the endpoint returns HTTP 200", async function (context) {
  const server = http.createServer(function handleRequest(_request, response) {
    response.writeHead(200);
    response.end();
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  context.after(function closeServer() {
    server.close();
  });

  const address = server.address();
  assert.notEqual(address, null);
  assert.notEqual(typeof address, "string");

  await execFileAsync("scripts/probe-health.sh", [], {
    env: {
      ...process.env,
      HEALTH_URL: `http://127.0.0.1:${address.port}/health`,
      MAX_ATTEMPTS: "1",
      RETRY_DELAY_SECONDS: "0",
    },
  });
});

test("production configuration reaches the Go service", function () {
  assert.equal(typeof createPrAgentEnvironment, "function");

  const bindings = Object.fromEntries([
    ["CF_ACCESS_CLIENT_ID", "fixture-a"],
    ["CF_ACCESS_CLIENT_SECRET", "fixture-b"],
    ["CLYDE_BASE_URL", "https://model.example/v1"],
    ["FALLBACK_API_KEY", "fixture-g"],
    ["FALLBACK_BASE_URL", "https://fallback.example/v1"],
    ["FALLBACK_CF_ACCESS_CLIENT_ID", "fixture-h"],
    ["FALLBACK_CF_ACCESS_CLIENT_SECRET", "fixture-i"],
    ["FALLBACK_MODEL", "fixture-fallback-model"],
    ["FALLBACK_ON", "usage_exceeded"],
    ["GITHUB_APP_ID", "fixture-c"],
    ["GITHUB_BOT_LOGIN", "fixture-bot[bot]"],
    ["GITHUB_PRIVATE_KEY", "fixture-d"],
    ["GITHUB_WEBHOOK_SECRET", "fixture-e"],
    ["OPENAI_KEY", "fixture-f"],
    ["PORT", "3000"],
    ["REVIEW_CHUNK_TIMEOUT", "5m"],
    ["REVIEW_MAX_CHUNKS", "60"],
    ["REVIEW_MAX_FILES", "100"],
    ["REVIEW_MIN_IMPORTANCE", "8"],
    ["REVIEW_MODEL", "fixture-review-model"],
    ["REVIEW_WORKERS", "5"],
  ]);
  const environment = createPrAgentEnvironment(bindings);

  assert.equal(environment.CLYDE_API_KEY, bindings.OPENAI_KEY);
  for (const name of Object.keys(bindings)) {
    if (name === "OPENAI_KEY") {
      continue;
    }
    assert.equal(environment[name], bindings[name], `${name} did not reach the service`);
  }
});

test("an unlisted binding never reaches the Go service", function () {
  const environment = createPrAgentEnvironment({ UNLISTED_BINDING: "fixture" });

  assert.equal("UNLISTED_BINDING" in environment, false);
});

test("every wrangler var reaches the Go service", function () {
  const config = JSON.parse(fs.readFileSync("wrangler.jsonc", "utf8"));
  const environment = createPrAgentEnvironment(config.vars);

  for (const name of Object.keys(config.vars)) {
    assert.equal(environment[name], config.vars[name], `${name} is declared but never forwarded`);
  }
});

test("release image selection writes the exact immutable digest", async function () {
  const temporaryDirectory = fs.mkdtempSync(path.join(os.tmpdir(), "pr-agent-cloudflare-"));
  const configPath = path.join(temporaryDirectory, "wrangler.jsonc");
  fs.copyFileSync("wrangler.jsonc", configPath);
  const image = `ghcr.io/agoodkind/pr-review-agent@sha256:${"a".repeat(64)}`;

  await execFileAsync(process.execPath, ["scripts/configure-image.mjs", configPath], {
    env: { ...process.env, PR_REVIEW_AGENT_IMAGE: image },
  });

  const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
  assert.equal(config.containers[0].image_vars.PR_REVIEW_AGENT_IMAGE, image);
});
