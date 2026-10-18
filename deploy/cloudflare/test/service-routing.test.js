import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHmac, randomUUID } from "node:crypto";
import { mkdtemp, writeFile } from "node:fs/promises";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { prepareServiceTestbed } from "../scripts/service-testbed.mjs";

const directory = fileURLToPath(new URL("..", import.meta.url));

async function unusedPort() {
  const server = net.createServer();
  await new Promise(function (resolve) { server.listen(0, "127.0.0.1", resolve); });
  const port = server.address().port;
  await new Promise(function (resolve) { server.close(resolve); });
  return port;
}

async function startWorkers(configurations, port, evidence, credentials) {
  const arguments_ = ["dev", "--local", "--port", String(port), "--inspector-port", "0",
    "--persist-to", path.join(evidence, "state")];
  for (const configuration of configurations) {
    arguments_.push("--config", configuration);
  }
  const child = spawn(path.join(directory, "node_modules/.bin/wrangler"), arguments_, {
    cwd: directory, stdio: ["ignore", "pipe", "pipe"],
  });
  let output = "";
  function sanitizedOutput() {
    let sanitized = output.split("\n").filter(function (line) {
      return !/env\..*(PRIVATE_KEY|SECRET|API_KEY|CLIENT_ID)/.test(line);
    }).join("\n");
    for (const value of credentials) {
      sanitized = sanitized.replaceAll(value, "[redacted]");
      sanitized = sanitized.replaceAll(JSON.stringify(value).slice(1, -1), "[redacted]");
    }
    return sanitized;
  }
  child.stdout.on("data", function (data) { output += data; });
  child.stderr.on("data", function (data) { output += data; });
  try {
    await new Promise(function (resolve, reject) {
      const deadline = setTimeout(function () {
        reject(new Error(`Worker startup timed out; private evidence: ${evidence}`));
      }, 120000);
      function receive() {
        if (output.includes("Ready on")) {
          clearTimeout(deadline);
          resolve();
        }
      }
      child.stdout.on("data", receive);
      child.stderr.on("data", receive);
      child.once("error", function (error) { clearTimeout(deadline); reject(error); });
      child.once("exit", function () {
        clearTimeout(deadline);
        reject(new Error(`Worker startup failed; private evidence: ${evidence}`));
      });
    });
  } catch (error) {
    if (child.exitCode === null && child.signalCode === null) {
      child.kill("SIGINT");
    }
    await writeFile(path.join(evidence, "worker.log"), sanitizedOutput(), { mode: 0o600 });
    throw error;
  }
  return async function stop() {
    if (child.exitCode === null && child.signalCode === null) {
      await new Promise(function (resolve) { child.once("exit", resolve); child.kill("SIGINT"); });
    }
    await writeFile(path.join(evidence, "worker.log"), sanitizedOutput(), { mode: 0o600 });
  };
}

async function assertHealthy(origin) {
  const deadline = Date.now() + 120000;
  let lastStatus;
  while (Date.now() < deadline) {
    const response = await fetch(origin + "/", { signal: AbortSignal.timeout(120000) });
    lastStatus = response.status;
    if (response.status === 200) {
      assert.match(response.headers.get("X-Pr-Agent-Revision") ?? "", /^[0-9a-f]{7,40}$/);
      assert.deepEqual(await response.json(), { status: "ok" });
      return response.headers.get("X-Pr-Agent-Revision");
    }
    await response.body?.cancel();
    await new Promise(function (resolve) { setTimeout(resolve, 500); });
  }
  assert.fail(`actual review service did not start; last HTTP status ${lastStatus}`);
}

test("native service bindings use the production review container and restrict candidate HTTP", {
  skip: process.env.PRAGENT_SERVICE_TEST_IMAGE || process.env.PRAGENT_SERVICE_TEST_BINARY ? false : "requires Docker and PRAGENT_SERVICE_TEST_IMAGE or PRAGENT_SERVICE_TEST_BINARY containing the production Linux service",
}, async function (context) {
  const evidence = await mkdtemp(path.join(os.tmpdir(), "pragent-service-routing-"));
  context.diagnostic(`private Worker integration evidence: ${evidence}`);
  const suffix = randomUUID().slice(0, 8);
  const { gateway, service, gatewayPath, servicePath, runtime, repository: fixture, recovery, deploymentToken, signingMaterial, redactions } = await prepareServiceTestbed({
    directory: evidence, image: process.env.PRAGENT_SERVICE_TEST_IMAGE, binary: process.env.PRAGENT_SERVICE_TEST_BINARY,
    account: "0".repeat(32), gatewayName: "pragent-gateway-routing-" + suffix, serviceName: "pragent-service-routing-" + suffix,
  });
  gateway.services = [{ binding: "REVIEW_SERVICE", service: service.name, entrypoint: "ReviewService" }];
  const port = await unusedPort();
  const origin = `http://127.0.0.1:${port}`;
  const proof = { gateway: gateway.name, service: service.name, phases: [] };
  function drainResponse(action) {
    return fetch(origin + "/internal/v1/drain", { method: "POST", body: JSON.stringify({ action }),
      headers: { Authorization: `Bearer ${deploymentToken}` } });
  }
  async function control(action) {
    const response = await drainResponse(action);
    assert.equal(response.status, 200, `actual service drain ${action} failed`);
    return response.json();
  }
  for (const mode of ["candidate", "service", "legacy", "idle"]) {
    if (mode === "idle") {
      runtime.CONTAINER_SLEEP_AFTER = "1s";
      await writeFile(path.join(evidence, "runtime.json"), JSON.stringify(runtime));
      await writeFile(path.join(fixture, "runtime.json"), JSON.stringify(runtime));
    }
    gateway.vars.REVIEW_SERVICE_MODE = mode === "legacy" ? "legacy" : "service";
    await writeFile(gatewayPath, JSON.stringify(gateway), { mode: 0o600 });
    const configurations = mode === "candidate" || mode === "idle" ? [servicePath, gatewayPath] : [gatewayPath, servicePath];
    const stop = await startWorkers(configurations, port, evidence, redactions);
    try {
      if (mode === "candidate") {
        const rejected = await fetch(origin + "/internal/v1/drain", { method: "POST", body: '{"action":"status"}' });
        assert.equal(rejected.status, 401);
        const stopped = await fetch(origin + "/internal/v1/drain", { method: "POST", body: '{"action":"status"}',
          headers: { Authorization: `Bearer ${deploymentToken}` } });
        assert.equal(stopped.status, 200);
        assert.deepEqual(await stopped.json(), { container_running: false });
      }
      const revision = await assertHealthy(origin);
      if (mode === "idle") {
        const deadline = Date.now() + 20000;
        let stopped = false;
        while (!stopped && Date.now() < deadline) {
          const response = await drainResponse("status");
          if (response.status === 200) {
            const state = await response.json();
            if (!state.container_running) {
              assert.deepEqual(state, { container_running: false });
              stopped = true;
            }
          } else {
            assert.equal(response.status, 503);
            await response.body?.cancel();
          }
          await new Promise(function (resolve) { setTimeout(resolve, 250); });
        }
        assert.equal(stopped, true, "atomic idle admission did not stop the empty service");
        await assertHealthy(origin);
        proof.phases.push({ mode, revision });
        continue;
      }
      if (mode === "candidate") {
        const open = await control("status");
        assert.deepEqual(open, { container_running: true, admission_closed: false, queued: 0, running: 0, outstanding: 0 });
        assert.deepEqual(await control("restart"), open);
        const closed = await control("begin");
        assert.deepEqual(closed, { ...open, admission_closed: true });
        assert.deepEqual(await control("begin"), closed);
        assert.deepEqual(await control("status"), closed);
        const unavailable = await fetch(origin + "/");
        assert.equal(unavailable.status, 503);
        await unavailable.body?.cancel();
        assert.deepEqual(await control("restart"), { container_running: false });
        await assertHealthy(origin);
        assert.deepEqual(await control("status"), open);
      }
      const body = "{}";
      const signature = "sha256=" + createHmac("sha256", signingMaterial).update(body).digest("hex");
      const response = await fetch(origin + "/api/v1/github_webhooks", { method: "POST", body,
        headers: { "X-Hub-Signature-256": signature, "X-Github-Event": "ping", "X-Github-Delivery": randomUUID() } });
      assert.equal(response.status, mode === "candidate" ? 404 : 202);
      if (mode !== "candidate") {
        assert.equal(response.headers.get("X-Pr-Agent-Delivery-State"), "settled");
        const recoveryBody = JSON.stringify({ action: "labeled",
          repository: { full_name: recovery.repository },
          pull_request: { number: recovery.pullRequest, labels: [{ name: recovery.forceLabel }] },
          label: { name: recovery.interruptLabel } });
        const recoverySignature = "sha256=" + createHmac("sha256", signingMaterial).update(recoveryBody).digest("hex");
        const interrupted = await fetch(origin + "/api/v1/github_webhooks", { method: "POST", body: recoveryBody,
          headers: { "X-Hub-Signature-256": recoverySignature, "X-Github-Event": "pull_request", "X-Github-Delivery": randomUUID() } });
        assert.equal(interrupted.status, 200);
        assert.deepEqual(await interrupted.json(), { status: "interrupted for recovery test" });
        await assertHealthy(origin);
      }
      const rpcAttempt = await fetch(origin + "/getConfiguration", { headers: { "X-RPC-Method": "getConfiguration" } });
      assert.equal(rpcAttempt.status, 404);
      proof.phases.push({ mode, revision });
    } finally {
      await stop();
    }
  }
  await writeFile(path.join(evidence, "proof.json"), JSON.stringify(proof, null, 2), { mode: 0o600 });
});
