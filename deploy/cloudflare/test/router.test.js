import assert from "node:assert/strict";
import { execFile } from "node:child_process";
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
