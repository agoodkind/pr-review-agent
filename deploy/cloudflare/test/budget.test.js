import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHmac } from "node:crypto";
import { mkdtemp, rm } from "node:fs/promises";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const signingKey = "test-review-budget-secret";
const cloudflareDirectory = fileURLToPath(new URL("..", import.meta.url));

async function unusedPort() {
  const server = net.createServer();
  await new Promise(function (resolve) {
    server.listen(0, "127.0.0.1", resolve);
  });
  const port = server.address().port;
  await new Promise(function (resolve) {
    server.close(resolve);
  });
  return port;
}

async function startRuntime(port, persistPath) {
  const wrangler = path.join(cloudflareDirectory, "node_modules/.bin/wrangler");
  const child = spawn(wrangler, [
    "dev", "--config", "test/budget.wrangler.jsonc", "--local",
    "--port", String(port), "--persist-to", persistPath,
  ], { cwd: cloudflareDirectory, stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  await new Promise(function (resolve, reject) {
    const timeout = setTimeout(function () {
      reject(new Error("budget worker did not start: " + output));
    }, 20000);
    function onOutput(data) {
      output += data.toString();
      if (output.includes("Ready on")) {
        clearTimeout(timeout);
        resolve();
      }
    }
    child.stdout.on("data", onOutput);
    child.stderr.on("data", onOutput);
    child.once("exit", function (code) {
      clearTimeout(timeout);
      reject(new Error("budget worker exited " + code + ": " + output));
    });
  });
  return child;
}

async function stopRuntime(child) {
  if (!child || child.exitCode !== null) {
    return;
  }
  child.kill("SIGINT");
  await new Promise(function (resolve) {
    child.once("exit", resolve);
  });
}

async function budgetRequest(port, payload) {
  const body = JSON.stringify(payload);
  const signature = "sha256=" + createHmac("sha256", signingKey).update(body).digest("hex");
  const response = await fetch(`http://127.0.0.1:${port}/internal/v1/provider_budget`, {
    method: "POST",
    headers: { "X-Pr-Agent-Budget-Signature": signature },
    body,
  });
  assert.equal(response.status, 200);
  return response.json();
}

function check(port, providerId) {
  return budgetRequest(port, { provider_id: providerId, limit: 2_000_000 });
}

function report(port, providerId, day, tokens) {
  return budgetRequest(port, { provider_id: providerId, day, tokens });
}

test("reported tokens set the daily provider limit and survive worker restart", async function () {
  const persistPath = await mkdtemp(path.join(os.tmpdir(), "pr-agent-budget-"));
  const port = await unusedPort();
  let runtime;
  try {
    runtime = await startRuntime(port, persistPath);
    const first = await check(port, "openai_goodkind_io");
    assert.equal(first.allowed, true);
    assert.match(first.day, /^\d{4}-\d{2}-\d{2}$/);
    assert.deepEqual(await report(port, "openai_goodkind_io", first.day, 1_500_000), { reported: true });
    assert.equal((await check(port, "openai_goodkind_io")).allowed, true);
    assert.deepEqual(await report(port, "openai_goodkind_io", first.day, 500_000), { reported: true });
    assert.equal((await check(port, "openai_goodkindalex")).allowed, true);
    assert.deepEqual(await budgetRequest(port, {
      provider_id: "legacy", tokens: 2_000_000, limit: 2_000_000,
    }), { allowed: true });

    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath);
    assert.equal((await check(port, "openai_goodkind_io")).allowed, false);
  } finally {
    await stopRuntime(runtime);
    await rm(persistPath, { recursive: true, force: true });
  }
});
