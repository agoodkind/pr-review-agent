import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash, createHmac, randomUUID } from "node:crypto";
import { mkdtemp, rm } from "node:fs/promises";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { durationMilliseconds } from "../worker/drift.js";

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
    "dev", "--config", "test/reassessment.wrangler.jsonc", "--local", "--port", String(port), "--inspector-port", "0", "--test-scheduled", "--persist-to", persistence,
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
    const completedNonce = randomUUID();
    const completed = { ...finished.record.outcome, nonce: completedNonce, head: finished.record.head, coverage_complete: true };
    const target = {
      key, version: 0, job: { InstallationID: 123, Repository: { Owner: "agoodkind", Name: "example" }, Number: 9, Head: finished.record.head },
      not_before_ms: finished.record.not_before_ms, private_outcome: completed,
      private_nonce: completedNonce, private_delivery_id: finished.record.attempt_id, private_generation: finished.record.generation,
      private_observed_at_ms: Date.now(), observation: {}, observed_at_ms: 0, failures: 0, confirmations: 0,
    };
    const receipt = await send(port, { action: "register_target", key, target });
    assert.equal(receipt.target.private_outcome.disposition, "completed");
    const failedNonce = randomUUID();
    const failed = { ...completed, nonce: failedNonce, disposition: "failed" };
    const replaced = await send(port, {
      action: "write_target", key, expected_version: receipt.target.version,
      expected_queue_version: finished.record.version, expected_queue_generation: finished.record.generation,
      target: { ...receipt.target, private_outcome: failed, private_nonce: failedNonce, private_observed_at_ms: Date.now() },
    });
    assert.equal(replaced.applied, true);
    const retried = await send(port, {
      action: "transition", key, expected_version: finished.record.version,
      record: { ...finished.record, phase: "waiting", reason: "failed", outcome: failed },
    });
    assert.equal(retried.applied, true);
    const latest = await send(port, { action: "query", key: "" }, true);
    assert.equal(latest.records[0].phase, "waiting");
    assert.equal(latest.records[0].outcome.disposition, "failed");
    assert.equal(latest.records[0].attempt_id, finished.record.attempt_id);
    assert.equal(latest.records[0].generation, finished.record.generation);
    assert.ok(latest.records[0].not_before_ms > Date.now());
    assert.equal(latest.targets[0].private_outcome.disposition, "failed");
    assert.equal(latest.targets[0].private_outcome.coverage_complete, true);
    assert.equal(latest.targets[0].private_nonce, failedNonce);
  } finally {
    await stopRuntime(runtime);
    await rm(persistence, { recursive: true, force: true });
  }
});

test("reconciliation intervals accept Go duration fractions", function () {
  assert.equal(durationMilliseconds(".5h"), 1800000);
  assert.equal(durationMilliseconds("+.5s"), 500);
  assert.equal(durationMilliseconds("1.h30m"), 5400000);
  assert.equal(durationMilliseconds("2µs"), 1);
  for (const invalid of [".h", "-1s", "0s", "1m trailing"]) {
    assert.throws(function parse() { durationMilliseconds(invalid); });
  }
});

test("scheduled inventory and private outcomes survive restart without webhooks", async function () {
  const persistence = await mkdtemp(path.join(os.tmpdir(), "pr-agent-registry-"));
  const port = await unusedPort();
  let runtime;
  try {
    runtime = await startRuntime(port, persistence);
    const empty = await send(port, { action: "query", key: "" }, true);
    assert.equal(empty.sweep, null);
    assert.deepEqual(empty.targets, []);
    const scheduled = await fetch(`http://127.0.0.1:${port}/__scheduled`);
    assert.equal(scheduled.status, 200);
    const initialized = await send(port, { action: "read_sweep" });
    assert.ok(initialized.sweep.version > 0);
    const delayed = { ...initialized.sweep, stage: 2, installation_ids: [123], repository_page: 3, not_before_ms: Date.now() + 86400000 };
    const savedSweep = await send(port, { action: "write_sweep", expected_version: initialized.sweep.version, sweep: delayed });
    assert.equal(savedSweep.applied, true);
    const key = "123:example/repository#9";
    const nonce = randomUUID();
    const target = {
      key, version: 0, job: { InstallationID: 123, Repository: { Owner: "example", Name: "repository" }, Number: 9, Head: "a".repeat(40) },
      not_before_ms: Date.now() + 86400000, private_outcome: { disposition: "completed", nonce, check_run_id: 71 },
      private_nonce: nonce, private_delivery_id: "delivery-owned", private_generation: "generation-a",
      private_observed_at_ms: Date.now(), observation: {}, observed_at_ms: 0, failures: 0, confirmations: 2,
    };
    const inserted = await send(port, { action: "register_target", key, target });
    assert.equal(inserted.target.version, 1);
    const registered = await send(port, { action: "register_target", key, target: { ...target, private_nonce: "", private_outcome: {}, not_before_ms: 0 } });
    assert.equal(registered.target.private_nonce, nonce);
    assert.equal(registered.target.not_before_ms, target.not_before_ms);
    const record = { id: "assessment-registry", key, generation: "generation-a", phase: "waiting", not_before_ms: target.not_before_ms, head: target.job.Head, attempts: 0 };
    const repaired = await send(port, { action: "apply_reconciliation", key, expected_target_version: 2, expected_queue_version: 0, queue_version_known: true, target: registered.target, record, repair_kind: "missing_queue" });
    assert.equal(repaired.applied, true);
    assert.equal(repaired.record.version, 1);
    const observed = await send(port, { action: "apply_reconciliation", key, expected_target_version: 3, expected_queue_version: 1, queue_version_known: true, target: repaired.target, record: null, repair_kind: "missing_queue" });
    assert.equal(observed.applied, true);
    assert.equal(observed.record.version, 1);
    const reactivated = await send(port, { action: "transition", key, expected_version: 1, record: { ...record, generation: "generation-b", reason: "reactivated" } });
    assert.equal(reactivated.applied, true);
    const staleOutcome = await send(port, { action: "write_target", key, expected_version: observed.target.version, expected_queue_version: 1, expected_queue_generation: "generation-a", target: { ...observed.target, private_nonce: "obsolete" } });
    assert.equal(staleOutcome.applied, false);
    assert.equal(staleOutcome.target.private_nonce, nonce);
    const staleRepair = await send(port, { action: "apply_reconciliation", key, expected_target_version: observed.target.version, expected_queue_version: 1, queue_version_known: true, target: observed.target, record: { ...record, phase: "terminal" }, repair_kind: "completed" });
    assert.equal(staleRepair.applied, false);
    const unchanged = await send(port, { action: "apply_reconciliation", key, expected_target_version: observed.target.version, expected_queue_version: 2, queue_version_known: true, target: observed.target, record: reactivated.record, repair_kind: "completed" });
    assert.equal(unchanged.applied, true);
    const deferred = await send(port, { action: "apply_reconciliation", key, expected_target_version: unchanged.target.version, expected_queue_version: 0, queue_version_known: false, target: { ...unchanged.target, failures: 1 }, record: null, repair_kind: "" });
    assert.equal(deferred.applied, true);
    assert.equal(deferred.target.failures, 1);
    assert.equal(deferred.record.generation, "generation-b");
    await send(port, { action: "apply_reconciliation", key, expected_target_version: deferred.target.version, expected_queue_version: 0, queue_version_known: false, target: deferred.target, record, repair_kind: "closed" }, false, 400);
    await send(port, { action: "register_target", key, target }, true, 403);
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistence);
    const snapshot = await send(port, { action: "query", key: "" }, true);
    assert.equal(snapshot.targets.length, 1);
    assert.equal(snapshot.targets[0].private_nonce, nonce);
    assert.equal(snapshot.targets[0].private_outcome.check_run_id, 71);
    assert.equal(snapshot.records[0].generation, "generation-b");
    assert.equal(snapshot.sweep.repository_page, 3);
    assert.ok(snapshot.next_alarm_ms > Date.now());
    assert.equal(snapshot.transition_counts.counts.missing_queue, 1);
    assert.equal(snapshot.transition_counts.counts.reactivated, 1);
    assert.equal(snapshot.transition_counts.counts.completed, 0);
    const sparse = await send(port, { action: "query_target", key: "absent" });
    assert.equal(sparse.target, null);
  } finally {
    await stopRuntime(runtime);
    await rm(persistence, { recursive: true, force: true });
  }
});
