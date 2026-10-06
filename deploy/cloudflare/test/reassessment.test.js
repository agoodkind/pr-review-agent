import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash, createHmac, randomUUID } from "node:crypto";
import { mkdtemp, rm } from "node:fs/promises";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const directory = fileURLToPath(new URL("..", import.meta.url));
const signingMaterial = randomUUID();
const operatorToken = randomUUID();

async function unusedPort() {
  const server = net.createServer();
  await new Promise(function listen(resolve) { server.listen(0, "127.0.0.1", resolve); });
  const port = server.address().port;
  await new Promise(function close(resolve) { server.close(resolve); });
  return port;
}

async function startRuntime(port, persistence) {
  const digest = createHash("sha256").update(operatorToken).digest("hex");
  const child = spawn(path.join(directory, "node_modules/.bin/wrangler"), [
    "dev", "--config", "test/reassessment.wrangler.jsonc", "--local", "--port", String(port), "--persist-to", persistence,
    "--var", `OPERATOR_TOKEN_SHA256:${digest}`,
    "--var", `GITHUB_WEBHOOK_SECRET:${signingMaterial}`,
  ], { cwd: directory, stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  try {
    await new Promise(function ready(resolve, reject) {
      const timeout = setTimeout(function expired() { reject(new Error("reassessment worker startup failed")); }, 20000);
      function receive(data) {
        output += data.toString();
        if (output.includes("Ready on")) { clearTimeout(timeout); resolve(); }
      }
      child.stdout.on("data", receive);
      child.stderr.on("data", receive);
      child.once("error", function failed(error) { clearTimeout(timeout); reject(error); });
      child.once("exit", function exited(code) { clearTimeout(timeout); reject(new Error(`reassessment worker exited ${code}`)); });
    });
  } catch (error) { await stopRuntime(child); throw error; }
  return child;
}

async function stopRuntime(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) { return; }
  await new Promise(function stop(resolve) { child.once("exit", resolve); child.kill("SIGINT"); });
}

async function send(port, payload, operator = false, expected = 200) {
  const body = JSON.stringify(payload);
  const headers = operator ? { Authorization: `Bearer ${operatorToken}` } : {
    "X-Pr-Agent-Reassessment-Signature": "sha256=" + createHmac("sha256", signingMaterial).update(body).digest("hex"),
  };
  const response = await fetch(`http://127.0.0.1:${port}/internal/v1/reassessments`, { method: "POST", headers, body });
  assert.equal(response.status, expected);
  if (response.ok) { return response.json(); }
  return null;
}

test("signed reassessment records survive restart and reject stale transitions", async function () {
  const persistence = await mkdtemp(path.join(os.tmpdir(), "pr-agent-reassessment-"));
  const port = await unusedPort();
  let runtime;
  try {
    runtime = await startRuntime(port, persistence);
    const key = "123:agoodkind/example#9";
    const record = { id: "assessment-a", key, generation: "generation-a", phase: "waiting", not_before_ms: Date.now() + 86400000, head: "a".repeat(40), attempts: 0 };
    const inserted = await send(port, { action: "transition", key, expected_version: 0, record });
    assert.equal(inserted.applied, true);
    assert.equal(inserted.record.version, 1);
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistence);
    const restored = await send(port, { action: "query", key }, true);
    assert.equal(restored.record.generation, "generation-a");
    const newer = { ...restored.record, generation: "generation-b", head: "b".repeat(40) };
    const updated = await send(port, { action: "transition", key, expected_version: 1, record: newer });
    assert.equal(updated.record.version, 2);
    const stale = await send(port, { action: "transition", key, expected_version: 1, record: { ...record, phase: "terminal" } });
    assert.equal(stale.applied, false);
    assert.equal(stale.record.head, "b".repeat(40));
    const snapshot = await send(port, { action: "query", key: "" }, true);
    assert.equal(snapshot.records.length, 1);
    assert.equal(snapshot.records[0].generation, "generation-b");
    await send(port, { action: "transition", key, expected_version: 2, record: newer }, true, 403);
    const unauthorized = await fetch(`http://127.0.0.1:${port}/internal/v1/reassessments`, { method: "POST", body: JSON.stringify({ action: "query", key }) });
    assert.equal(unauthorized.status, 401);
    assert.equal((await send(port, { action: "query", key }, true)).record.version, 2);
    const running = { ...newer, phase: "running", attempt_id: "attempt-b", attempts: 1 };
    await send(port, { action: "transition", key, expected_version: 2, record: running });
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistence);
    const resumed = await send(port, { action: "query", key }, true);
    assert.equal(resumed.record.phase, "running");
    assert.equal(resumed.record.attempt_id, "attempt-b");
    assert.equal(resumed.record.attempts, 1);
    const confirming = { ...resumed.record, phase: "confirming", outcome: { disposition: "completed", check_run_id: 71 } };
    await send(port, { action: "transition", key, expected_version: 3, record: confirming });
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistence);
    const finished = await send(port, { action: "query", key }, true);
    assert.equal(finished.record.outcome.check_run_id, 71);
    assert.equal(finished.record.phase, "confirming");
    assert.equal(finished.record.attempt_id, "attempt-b");
  } finally {
    await stopRuntime(runtime);
    await rm(persistence, { recursive: true, force: true });
  }
});
