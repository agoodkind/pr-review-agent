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
let signReviewSettings;
const execFileAsync = promisify(execFile);

try {
  ({ routeRequest } = await import("../worker/router.js"));
} catch {}

try {
  ({ createPrAgentEnvironment, signReviewSettings } = await import("../worker/configuration.js"));
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

// createForwardingEnvironment answers every forward and records that it
// happened, with a replay queue bound so a test can prove a delivery did not
// reach it. Without the binding, a queued delivery would fail on the missing
// binding and read as some other fault.
function createForwardingEnvironment(events, queued) {
  return {
    GITHUB_WEBHOOK_SECRET: testWebhookSecret, // gitleaks:allow
    PR_AGENT: {
      getByName(name) {
        assert.equal(name, "github-app");
        return {
          async fetch() {
            events.push("forward");
            return new Response("proxied", { status: 202 });
          },
        };
      },
    },
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

function labeledWebhookRequest(action, labelName, signature) {
  const body = JSON.stringify({
    action,
    label: { name: labelName },
    pull_request: { draft: false, head: { sha: "e6949cd" } },
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

// This worker decides nothing about a delivery, so a forcing label reaches the
// container exactly as every other delivery does. Nothing here inspects the
// label, and nothing restarts anything: a restart takes down whatever reviews
// are in flight, and the label asks for a full review rather than for other
// people's work to be killed.
test("a forcing label is forwarded like any other delivery", async function () {
  const cases = [
    ["labeled", "test-review-agent-rerun"],
    ["labeled", "needs-review"],
    ["unlabeled", "test-review-agent-rerun"],
    ["synchronize", ""],
    ["opened", ""],
  ];

  for (const [action, labelName] of cases) {
    const events = [];
    const queued = [];

    const response = await routeRequest(
      labeledWebhookRequest(action, labelName),
      createForwardingEnvironment(events, queued),
    );

    assert.equal(response.status, 202, `${action} ${labelName}`);
    assert.deepEqual(events, ["forward"], `${action} ${labelName} did not reach the container`);
    assert.equal(queued.length, 0, `${action} ${labelName} was queued`);
  }
});

// The container reads its configuration once, at start, so a value corrected
// after it booted did not reach a running review. The worker attaches the
// current values to every delivery instead, and the service applies them once
// the signature on the body has verified.
//
// Only values that govern one review travel. A secret on every forwarded request
// would be a far worse trade than the staleness it fixed.
test("every forwarded delivery carries the review tuning values and no secret", async function () {
  const events = [];
  let forwarded = null;
  const environment = createForwardingEnvironment(events, []);
  environment.REVIEW_MIN_IMPORTANCE = "6";
  environment.REVIEW_MAX_FILES = "120";
  environment.REVIEW_MAX_CHUNKS = "70";
  environment.REVIEW_CHUNK_TIMEOUT = "4m";
  environment.GITHUB_PRIVATE_KEY = "a private key nobody may forward";
  environment.OPENAI_KEY = "a model key nobody may forward";
  environment.PR_AGENT = {
    getByName() {
      return {
        async fetch(request) {
          forwarded = request;
          events.push("forward");
          return new Response("proxied", { status: 202 });
        },
      };
    },
  };

  const response = await routeRequest(labeledWebhookRequest("opened", ""), environment);

  assert.equal(response.status, 202);
  assert.deepEqual(events, ["forward"]);
  const settings = JSON.parse(forwarded.headers.get("X-Pr-Agent-Review-Settings"));
  assert.deepEqual(settings, {
    minimum_importance: 6,
    max_files: 120,
    max_chunks: 70,
    chunk_timeout: "4m",
  });

  const headerText = JSON.stringify([...forwarded.headers]);
  assert.doesNotMatch(headerText, /private key nobody may forward/);
  assert.doesNotMatch(headerText, /model key nobody may forward/);

  // The values carry their own signature, over themselves and the body, because
  // the webhook signature covers the body alone and says nothing about a header
  // travelling beside it.
  const expected = await signReviewSettings(
    testWebhookSecret, // gitleaks:allow
    forwarded.headers.get("X-Pr-Agent-Review-Settings"),
    await forwarded.clone().text(),
  );
  assert.equal(forwarded.headers.get("X-Pr-Agent-Review-Settings-Signature"), expected);
});

// A binding this worker cannot use is left out rather than forwarded. It would
// otherwise ride every delivery and make the service reject or instantly time
// out every review it governs, and the service reads an absent field as its own
// configuration standing, which is the answer a misconfigured binding deserves.
test("a chunk timeout binding that is not a positive duration is not forwarded", async function () {
  for (const value of ["", "soon", "0s", "0m0s", "0h0m0s", "-5m", "5", "5 m"]) {
    const environment = createForwardingEnvironment([], []);
    environment.REVIEW_CHUNK_TIMEOUT = value;
    let forwarded = null;
    environment.PR_AGENT = {
      getByName() {
        return {
          async fetch(request) {
            forwarded = request;
            return new Response("proxied", { status: 202 });
          },
        };
      },
    };

    const response = await routeRequest(labeledWebhookRequest("opened", ""), environment);
    assert.equal(response.status, 202);

    const header = forwarded.headers.get("X-Pr-Agent-Review-Settings");
    const settings = header === null ? {} : JSON.parse(header);
    assert.equal(
      "chunk_timeout" in settings,
      false,
      `${JSON.stringify(value)} was forwarded as a chunk timeout`,
    );
  }
});

// A sender must not be able to name its own tuning values by finding a worker
// that has none of its own. Both headers are stripped on every path, including
// the one where this worker sends nothing, so the only way in is the one that
// gets signed.
test("an inbound settings header is stripped whether or not the worker sends its own", async function () {
  for (const configured of [true, false]) {
    const events = [];
    let forwarded = null;
    const environment = createForwardingEnvironment(events, []);
    if (configured) {
      environment.REVIEW_MIN_IMPORTANCE = "6";
    }
    environment.PR_AGENT = {
      getByName() {
        return {
          async fetch(request) {
            forwarded = request;
            events.push("forward");
            return new Response("proxied", { status: 202 });
          },
        };
      },
    };

    const inbound = labeledWebhookRequest("opened", "");
    inbound.headers.set("X-Pr-Agent-Review-Settings", '{"minimum_importance":1,"chunk_timeout":"1ms"}');
    inbound.headers.set("X-Pr-Agent-Review-Settings-Signature", "sha256=" + "0".repeat(64));

    const response = await routeRequest(inbound, environment);

    const where = `configured=${configured}`;
    assert.equal(response.status, 202, where);
    const settings = forwarded.headers.get("X-Pr-Agent-Review-Settings");
    assert.doesNotMatch(String(settings), /1ms/, `${where}: the caller's values survived`);
    if (!configured) {
      assert.equal(settings, null, where);
      assert.equal(forwarded.headers.get("X-Pr-Agent-Review-Settings-Signature"), null, where);
    }
  }
});

// A worker holding no signing key cannot authenticate what it sends, and an
// unauthenticated value here is one anybody could have chosen, so it sends none.
test("a worker with no signing key attaches no settings", async function () {
  const events = [];
  let forwarded = null;
  const environment = createForwardingEnvironment(events, []);
  environment.REVIEW_MIN_IMPORTANCE = "6";
  delete environment.GITHUB_WEBHOOK_SECRET;
  environment.PR_AGENT = {
    getByName() {
      return {
        async fetch(request) {
          forwarded = request;
          events.push("forward");
          return new Response("proxied", { status: 202 });
        },
      };
    },
  };

  const response = await routeRequest(labeledWebhookRequest("opened", ""), environment);

  assert.equal(response.status, 202);
  assert.equal(forwarded.headers.get("X-Pr-Agent-Review-Settings"), null);
  assert.equal(forwarded.headers.get("X-Pr-Agent-Review-Settings-Signature"), null);
});

// A worker with nothing configured must send nothing, because the service reads
// an absent header as its own configuration standing. That is what lets a worker
// and a container at different versions work together.
//
// A binding that is not a whole number above zero counts as nothing configured.
// A zero or a negative would disable a budget, and leaving it out says so before
// the service has to refuse it.
test("a worker with no usable tuning values attaches no header", async function () {
  for (const bindings of [
    {},
    { REVIEW_MAX_CHUNKS: "0" },
    { REVIEW_MIN_IMPORTANCE: "-1" },
    { REVIEW_MAX_FILES: "not a number" },
    { REVIEW_CHUNK_TIMEOUT: "" },
  ]) {
    const events = [];
    let forwarded = null;
    const environment = createForwardingEnvironment(events, []);
    Object.assign(environment, bindings);
    environment.PR_AGENT = {
      getByName() {
        return {
          async fetch(request) {
            forwarded = request;
            events.push("forward");
            return new Response("proxied", { status: 202 });
          },
        };
      },
    };

    const response = await routeRequest(labeledWebhookRequest("opened", ""), environment);

    const where = JSON.stringify(bindings);
    assert.equal(response.status, 202, where);
    assert.equal(forwarded.headers.get("X-Pr-Agent-Review-Settings"), null, where);
  }
});

// The metadata is built from a body nobody has verified and goes straight into
// a log line, so what a stranger puts in the payload must not be able to throw
// there. It once did, when a reader called a string method on the label.
//
// That reader is gone with the restart, so this asserts the narrowing where it
// is still observable: the line the worker logs carries a string for the label
// whatever the payload held.
test("a label name that is not a string reaches the log as a string", async function () {
  for (const labelName of [12345, 3.14, true, { nested: "object" }, ["array"]]) {
    const events = [];
    const logged = [];
    const realLog = console.log;
    console.log = function (line) {
      logged.push(line);
    };

    let response;
    try {
      response = await routeRequest(
        labeledWebhookRequest("labeled", labelName),
        createForwardingEnvironment(events, []),
      );
    } finally {
      console.log = realLog;
    }

    const where = `label ${JSON.stringify(labelName)}`;
    assert.equal(response.status, 202, where);
    assert.deepEqual(events, ["forward"], where);
    const forwarding = logged.find(function (line) {
      return line.includes('"message":"webhook forwarding"');
    });
    assert.ok(forwarding, `${where}: no forwarding line was logged`);
    assert.equal(JSON.parse(forwarding).label, "", where);
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
