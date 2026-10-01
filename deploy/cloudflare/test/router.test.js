import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { once } from "node:events";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";

let createPrAgentEnvironment;
const execFileAsync = promisify(execFile);

try {
  ({ createPrAgentEnvironment } = await import("../worker/configuration.js"));
} catch {}

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

test("an unlisted binding never reaches the Go service", function () {
  const environment = createPrAgentEnvironment({ UNLISTED_BINDING: "fixture" });

  assert.equal("UNLISTED_BINDING" in environment, false);
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
