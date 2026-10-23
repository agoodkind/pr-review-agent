import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { createHmac, randomUUID } from "node:crypto";
import { mkdtemp, readFile, rename, rm, unlink, writeFile } from "node:fs/promises";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { prepareServiceTestbed } from "../scripts/service-testbed.mjs";

const directory = fileURLToPath(new URL("..", import.meta.url));

async function writeGatewayConfiguration(file, configuration) {
  const temporary = file + "." + randomUUID() + ".tmp";
  await writeFile(temporary, JSON.stringify(configuration), { mode: 0o600, flag: "wx" });
  await rename(temporary, file);
}

async function unusedPort() {
  const server = net.createServer();
  await new Promise(function (resolve) { server.listen(0, "127.0.0.1", resolve); });
  const port = server.address().port;
  await new Promise(function (resolve) { server.close(resolve); });
  return port;
}

async function startWorkers(configurations, port, evidence, credentials, phase) {
  const arguments_ = ["dev", "--local", "--port", String(port), "--inspector-port", "0",
    "--persist-to", path.join(evidence, "state")];
  for (const configuration of configurations) {
    arguments_.push("--config", configuration);
  }
  const child = spawn(path.join(directory, "node_modules/.bin/wrangler"), arguments_, {
    cwd: directory, stdio: ["ignore", "pipe", "pipe"],
    // Poll the configuration files this fixture replaces during reloads.
    env: { ...process.env, CHOKIDAR_USEPOLLING: "true" },
  });
  let output = "";
  const logPath = path.join(evidence, phase + ".log");
  function sanitizeOutput(text) {
    let binding = false;
    let sanitized = text.split("\n").filter(function (line) {
      if (/^env\./.test(line)) binding = true;
      if (!binding) return true;
      if (/\b(local|remote)(?: \[.*\])?\s*$/.test(line)) binding = false;
      return false;
    }).join("\n");
    for (const value of credentials) {
      sanitized = sanitized.replaceAll(value, "[redacted]");
      sanitized = sanitized.replaceAll(JSON.stringify(value).slice(1, -1), "[redacted]");
    }
    return sanitized;
  }
  function sanitizedOutput() { return sanitizeOutput(output); }
  child.stdout.on("data", function (data) { output += data; });
  child.stderr.on("data", function (data) { output += data; });
  const evidenceTimer = setInterval(function () {
    void writeFile(logPath, sanitizedOutput(), { mode: 0o600 });
  }, 1000);
  evidenceTimer.unref();
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
    clearInterval(evidenceTimer);
    if (child.exitCode === null && child.signalCode === null) {
      child.kill("SIGINT");
    }
    await writeFile(logPath, sanitizedOutput(), { mode: 0o600 });
    throw error;
  }
  const stop = async function stop() {
    clearInterval(evidenceTimer);
    if (child.exitCode === null && child.signalCode === null) {
      await new Promise(function (resolve) { child.once("exit", resolve); child.kill("SIGINT"); });
    }
    await writeFile(logPath, sanitizedOutput(), { mode: 0o600 });
  };
  stop.readOutput = sanitizedOutput;
  stop.sanitize = sanitizeOutput;
  return stop;
}

async function boundedErrorBody(response) {
  if (!response.body) return "";
  const reader = response.body.getReader();
  const chunks = [];
  let remaining = 4096;
  try {
    while (remaining > 0) {
      const { done, value } = await reader.read();
      if (done) break;
      const part = value.subarray(0, remaining);
      chunks.push(part);
      remaining -= part.byteLength;
    }
  } finally {
    await reader.cancel();
  }
  return Buffer.concat(chunks).toString("utf8");
}

function dockerDiagnostic(arguments_) {
  return new Promise(function (resolve) {
    execFile("docker", arguments_, { timeout: 5000, maxBuffer: 16384, encoding: "utf8" }, function (error, stdout, stderr) {
      resolve({ error: error ? String(error) : null, output: (stdout + stderr).slice(-6000) });
    });
  });
}

async function containerDiagnostics(gatewayName, serviceName) {
  const listed = await dockerDiagnostic(["ps", "-a", "--filter", `name=workerd-${gatewayName}-`,
    "--filter", `name=workerd-${serviceName}-`, "--format", "{{.Names}}"]);
  if (listed.error) return listed;
  const names = listed.output.trim().split("\n").filter(function (name) {
    return (name.startsWith(`workerd-${gatewayName}-`) || name.startsWith(`workerd-${serviceName}-`)) && !name.endsWith("-proxy");
  }).slice(0, 3);
  return Promise.all(names.map(async function (name) {
    return { name, ...await dockerDiagnostic(["logs", "--tail", "40", name]) };
  }));
}

async function waitForHandoffRecoveryReceipt(signalFile, lifetime) {
  const signal = AbortSignal.any([lifetime, AbortSignal.timeout(120000)]);
  try {
    while (true) {
      signal.throwIfAborted();
      try {
        const receipt = await readFile(signalFile, "utf8");
        assert.equal(receipt, "response_received\n", "native handoff recovery receipt is invalid");
        return;
      } catch (error) {
        if (error.code !== "ENOENT") throw error;
      }
      await delay(50, undefined, { signal });
    }
  } catch (error) {
    throw new Error(`Native handoff recovery receipt did not become available: ${signalFile}`, { cause: error });
  }
}

function runNativeDeploymentTest(testName, logPath, environment, deploymentToken, signal) {
  return new Promise(function (resolve, reject) {
    execFile("go", ["test", "-tags=integration", "./internal/cloudflareops", "-run", "^" + testName + "$", "-count=1", "-v", "-timeout=150s"], {
      cwd: path.resolve(directory, "../.."), timeout: 180000, maxBuffer: 32768, encoding: "utf8", signal,
      env: { ...process.env, ...environment },
    }, function (error, stdout, stderr) {
      const diagnosticOutput = (stdout + stderr).replaceAll(deploymentToken, "[redacted]");
      void writeFile(logPath, diagnosticOutput, { mode: 0o600 }).then(function () {
        if (error) reject(new Error(`native deployment control regression failed: ${diagnosticOutput}`));
        else resolve();
      }, reject);
    });
  });
}

async function assertHandoffConflictRetries(origin, revision, deploymentToken, evidence) {
  const tokenFile = path.join(evidence, "handoff-control-test-token");
  const lifetime = new AbortController();
  await writeFile(tokenFile, deploymentToken, { mode: 0o600, flag: "wx" });
  try {
    await runNativeDeploymentTest("TestGatewayHandoffConflictRetriesNative", path.join(evidence, "handoff-control-test.log"), {
      PRAGENT_HANDOFF_TEST_ORIGIN: origin, PRAGENT_HANDOFF_TEST_REVISION: revision,
      PRAGENT_HANDOFF_TEST_TOKEN_FILE: tokenFile,
    }, deploymentToken, lifetime.signal);
  } finally {
    lifetime.abort();
    await unlink(tokenFile);
  }
}

async function assertHandoffReadinessRetries(origin, revision, deploymentToken, evidence, logName, restore) {
  const tokenFile = path.join(evidence, logName + "-token");
  const signalFile = path.join(evidence, logName + "-recovery-signal");
  const lifetime = new AbortController();
  await writeFile(tokenFile, deploymentToken, { mode: 0o600, flag: "wx" });
  const completion = runNativeDeploymentTest("TestGatewayHandoffReadinessRetriesNative", path.join(evidence, logName + ".log"), {
    PRAGENT_HANDOFF_TEST_ORIGIN: origin, PRAGENT_HANDOFF_TEST_REVISION: revision,
    PRAGENT_HANDOFF_TEST_TOKEN_FILE: tokenFile, PRAGENT_HANDOFF_TEST_RECOVERY_SIGNAL_FILE: signalFile,
  }, deploymentToken, lifetime.signal);
  const restoration = waitForHandoffRecoveryReceipt(signalFile, lifetime.signal).then(restore);
  try {
    await Promise.all([completion, restoration]);
  } finally {
    lifetime.abort();
    await Promise.allSettled([completion, restoration]);
    await unlink(tokenFile);
    await rm(signalFile, { force: true });
    await rm(signalFile + ".tmp", { force: true });
  }
}

async function assertCandidateControlRetries(testName, origin, revision, name, deploymentToken, evidence, restore) {
  const tokenFile = path.join(evidence, testName + "-token");
  const signalFile = path.join(evidence, testName + "-recovery-signal");
  const lifetime = new AbortController();
  await writeFile(tokenFile, deploymentToken, { mode: 0o600, flag: "wx" });
  const completion = runNativeDeploymentTest(testName, path.join(evidence, testName + ".log"), {
    PRAGENT_HANDOFF_TEST_ORIGIN: origin, PRAGENT_HANDOFF_TEST_REVISION: revision,
    PRAGENT_CANDIDATE_TEST_NAME: name, PRAGENT_HANDOFF_TEST_TOKEN_FILE: tokenFile,
    PRAGENT_HANDOFF_TEST_RECOVERY_SIGNAL_FILE: signalFile,
  }, deploymentToken, lifetime.signal);
  const restoration = waitForHandoffRecoveryReceipt(signalFile, lifetime.signal).then(restore);
  try {
    await Promise.all([completion, restoration]);
  } finally {
    lifetime.abort();
    await Promise.allSettled([completion, restoration]);
    await unlink(tokenFile);
    await rm(signalFile, { force: true });
    await rm(signalFile + ".tmp", { force: true });
  }
}

async function assertHealthy(origin, expectedRevision, failureEvidence) {
  const deadline = Date.now() + 120000;
  let lastStatus;
  let lastBody = "";
  let lastRevision = "";
  while (Date.now() < deadline) {
    const response = await fetch(origin + "/", { signal: AbortSignal.timeout(120000) });
    lastStatus = response.status;
    lastRevision = response.headers.get("X-Pr-Agent-Revision") ?? "";
    if (response.status === 200) {
      if (lastRevision !== expectedRevision && failureEvidence) {
        lastBody = await boundedErrorBody(response);
        const evidence = await failureEvidence({ lastStatus, lastBody, lastRevision });
        assert.equal(lastRevision, expectedRevision, JSON.stringify(evidence));
      }
      assert.equal(response.headers.get("X-Pr-Agent-Revision"), expectedRevision);
      assert.deepEqual(await response.json(), { status: "ok" });
      return response.headers.get("X-Pr-Agent-Revision");
    }
    lastBody = await boundedErrorBody(response);
    await new Promise(function (resolve) { setTimeout(resolve, 500); });
  }
  const evidence = failureEvidence ? await failureEvidence({ lastStatus, lastBody, lastRevision }) : { lastStatus, lastBody, lastRevision };
  assert.fail(`review service health check did not become ready: ${JSON.stringify(evidence)}`);
}

test("native service bindings use the production review container and restrict candidate HTTP", {
  skip: process.env.PRAGENT_SERVICE_TEST_IMAGE || process.env.PRAGENT_SERVICE_TEST_BINARY ? false : "requires Docker and PRAGENT_SERVICE_TEST_IMAGE or PRAGENT_SERVICE_TEST_BINARY containing the production Linux service",
}, async function (context) {
  const expectedRevision = process.env.PRAGENT_SERVICE_TEST_REVISION ?? process.env.GITHUB_SHA;
  assert.match(expectedRevision ?? "", /^[0-9a-f]{40}$/, "set PRAGENT_SERVICE_TEST_REVISION to the actual binary source revision");
  const phases = ["candidate", "service", "legacy", "idle", "legacy-handoff", "service-handoff"];
  const selectedPhases = new Set((process.env.PRAGENT_SERVICE_TEST_PHASES ?? phases.join(",")).split(","));
  for (const phase of selectedPhases) {
    assert.ok(phases.includes(phase), `unknown service integration phase: ${phase}`);
  }
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
  const originalSleep = runtime.CONTAINER_SLEEP_AFTER;
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
    if (!selectedPhases.has(mode)) continue;
    if (mode === "idle") {
      runtime.CONTAINER_SLEEP_AFTER = "1s";
      await writeFile(path.join(evidence, "runtime.json"), JSON.stringify(runtime));
      await writeFile(path.join(fixture, "runtime.json"), JSON.stringify(runtime));
    }
    gateway.vars.REVIEW_SERVICE_MODE = mode === "legacy" ? "legacy" : "service";
    await writeGatewayConfiguration(gatewayPath, gateway);
    const configurations = mode === "candidate" || mode === "idle" ? [servicePath, gatewayPath] : [gatewayPath, servicePath];
    const stop = await startWorkers(configurations, port, evidence, redactions, mode);
    try {
      if (mode === "candidate") {
        const rejected = await fetch(origin + "/internal/v1/drain", { method: "POST", body: '{"action":"status"}' });
        assert.equal(rejected.status, 401);
        const stopped = await fetch(origin + "/internal/v1/drain", { method: "POST", body: '{"action":"status"}',
          headers: { Authorization: `Bearer ${deploymentToken}` } });
        assert.equal(stopped.status, 200);
        assert.deepEqual(await stopped.json(), { container_running: false });
      }
      const revision = await assertHealthy(origin, expectedRevision);
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
        await assertHealthy(origin, expectedRevision);
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
        await assertHealthy(origin, expectedRevision);
        assert.deepEqual(await control("status"), open);
        const runtimePath = path.join(fixture, "runtime.json");
        async function assertUnavailableControl(testName) {
          let runtimeRestored = false;
          async function restoreRuntime() {
            await writeGatewayConfiguration(runtimePath, runtime);
            runtimeRestored = true;
          }
          try {
            await writeGatewayConfiguration(runtimePath, { ...runtime, PORT: "0" });
            const deadline = Date.now() + 120000;
            let unavailable = false;
            while (Date.now() < deadline) {
              const response = await drainResponse("status");
              await response.body?.cancel();
              if (response.status === 503) {
                unavailable = true;
                break;
              }
              assert.equal(response.status, 200);
              await delay(100);
            }
            assert.equal(unavailable, true, "invalid configuration did not produce actual candidate control HTTP 503");
            await assertCandidateControlRetries(testName, origin, expectedRevision, service.name, deploymentToken, evidence, restoreRuntime);
          } finally {
            if (!runtimeRestored) await restoreRuntime();
          }
        }
        await assertUnavailableControl("TestCandidateDeploymentControlRetriesNative");
        await assertHealthy(origin, expectedRevision);
        assert.deepEqual(await control("status"), open);
        proof.candidateControlRetried = true;
        assert.deepEqual(await control("begin"), closed);
        await assertUnavailableControl("TestCandidateRestartRetriesNative");
        assert.deepEqual(await control("status"), { container_running: false });
        await assertHealthy(origin, expectedRevision);
        assert.deepEqual(await control("status"), open);
        proof.candidateRestartRetried = true;
        await assertUnavailableControl("TestCandidateBeginRetriesNative");
        assert.deepEqual(await control("status"), closed);
        assert.deepEqual(await control("restart"), { container_running: false });
        await assertHealthy(origin, expectedRevision);
        assert.deepEqual(await control("status"), open);
        proof.candidateBeginRetried = true;
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
        await assertHealthy(origin, expectedRevision);
      }
      const rpcAttempt = await fetch(origin + "/getConfiguration", { headers: { "X-RPC-Method": "getConfiguration" } });
      assert.equal(rpcAttempt.status, 404);
      proof.phases.push({ mode, revision });
    } finally {
      await stop();
    }
  }
  runtime.CONTAINER_SLEEP_AFTER = originalSleep;
  await writeFile(path.join(evidence, "runtime.json"), JSON.stringify(runtime));
  await writeFile(path.join(fixture, "runtime.json"), JSON.stringify(runtime));
  const nextService = structuredClone(service);
  nextService.name += "-next";
  service.dev.port = await unusedPort();
  nextService.dev.port = await unusedPort();
  const nextServicePath = path.join(evidence, "service-next.json");
  await writeFile(servicePath, JSON.stringify(service), { mode: 0o600 });
  await writeFile(nextServicePath, JSON.stringify(nextService), { mode: 0o600 });
  gateway.vars.DEPLOYMENT_TOKEN_SHA256 = service.vars.DEPLOYMENT_TOKEN_SHA256;
  for (const source of ["legacy", "service"]) {
    if (!selectedPhases.has(source + "-handoff")) continue;
    const candidate = source === "legacy" ? service : nextService;
    gateway.vars.REVIEW_SERVICE_MODE = source;
    gateway.vars.REVIEW_HANDOFF = "false";
    gateway.vars.REVIEW_HANDOFF_REVISION = "0".repeat(40);
    gateway.services = [
      { binding: "REVIEW_SERVICE", service: service.name, entrypoint: "ReviewService" },
      { binding: "CANDIDATE_REVIEW_SERVICE", service: candidate.name, entrypoint: "ReviewService" },
    ];
    await writeGatewayConfiguration(gatewayPath, gateway);
    const stop = await startWorkers([gatewayPath, servicePath, nextServicePath], port, evidence, redactions, source + "-handoff");
    async function failureEvidence(lastResponse) {
      const diagnostic = { phase: source + "-handoff", expectedRevision, ...lastResponse };
      try {
        const response = await fetch(origin + "/internal/v1/deployment", { method: "POST",
          signal: AbortSignal.timeout(5000), headers: { Authorization: `Bearer ${deploymentToken}` },
          body: JSON.stringify({ action: "status", expected_revision: gateway.vars.REVIEW_HANDOFF_REVISION }) });
        diagnostic.deployment = { status: response.status, body: await boundedErrorBody(response) };
      } catch (error) {
        diagnostic.deployment = { error: String(error) };
      }
      diagnostic.workerOutput = stop.readOutput().split("\n").slice(-120).join("\n").slice(-12000);
      diagnostic.containers = await containerDiagnostics(gateway.name, service.name);
      const sanitized = stop.sanitize(JSON.stringify(diagnostic));
      await writeFile(path.join(evidence, source + "-handoff-failure.json"), sanitized, { mode: 0o600 });
      return JSON.parse(sanitized);
    }
    async function stateWhen(expected, predicate) {
      const deadline = Date.now() + 120000;
      let lastState;
      while (Date.now() < deadline) {
        const response = await fetch(origin + "/internal/v1/deployment", { method: "POST",
          headers: { Authorization: `Bearer ${deploymentToken}` },
          body: JSON.stringify({ action: "status", expected_revision: expected }) });
        if (response.ok) {
          const state = await response.json();
          lastState = state;
          if (predicate(state)) return state;
        } else {
          assert.ok([409, 503].includes(response.status), `deployment status failed with HTTP ${response.status}`);
          await response.body?.cancel();
        }
        await new Promise(function (resolve) { setTimeout(resolve, 250); });
      }
      assert.fail(`actual deployment state did not satisfy the ${source} handoff assertion: ${JSON.stringify(lastState)}`);
    }
    try {
      const unauthorized = await fetch(origin + "/internal/v1/deployment", { method: "POST",
        body: JSON.stringify({ action: "status", expected_revision: expectedRevision }) });
      assert.equal(unauthorized.status, 401);
      await unauthorized.body?.cancel();
      await assertHealthy(origin, expectedRevision, failureEvidence);
      const before = await stateWhen(expectedRevision, function (state) { return !state.handoff; });
      assert.equal(before.previous_container_running, true);
      assert.equal(before.complete, false);
      if (source === "service") {
        await assertHandoffConflictRetries(origin, expectedRevision, deploymentToken, evidence);
      }
      gateway.vars.REVIEW_HANDOFF = "true";
      await writeGatewayConfiguration(gatewayPath, gateway);
      await stateWhen("0".repeat(40), function (state) { return state.handoff; });
      const wrongCandidate = await fetch(origin + "/");
      assert.equal(wrongCandidate.status, 503);
      await wrongCandidate.body?.cancel();
      const stillRunning = await stateWhen("0".repeat(40), function (state) { return state.handoff; });
      assert.equal(stillRunning.previous_container_running, true);
      assert.equal(stillRunning.candidate_ready, false);
      const body = "{}";
      const deliveryId = randomUUID();
      const queued = await fetch(origin + "/api/v1/github_webhooks", { method: "POST", body,
        headers: { "X-Hub-Signature-256": "sha256=" + createHmac("sha256", signingMaterial).update(body).digest("hex"),
          "X-Github-Event": "ping", "X-Github-Delivery": deliveryId } });
      assert.equal(queued.status, 202);
      assert.deepEqual(await queued.json(), { status: "queued for replay", deliveryId });
      const recoveryBody = JSON.stringify({ action: "labeled", repository: { full_name: recovery.repository },
        pull_request: { number: recovery.pullRequest, labels: [{ name: recovery.forceLabel }] },
        label: { name: recovery.interruptLabel } });
      const interrupted = await fetch(origin + "/api/v1/github_webhooks", { method: "POST", body: recoveryBody,
        headers: { "X-Hub-Signature-256": "sha256=" + createHmac("sha256", signingMaterial).update(recoveryBody).digest("hex"),
          "X-Github-Event": "pull_request", "X-Github-Delivery": randomUUID() } });
      assert.equal(interrupted.status, 503);
      await interrupted.body?.cancel();
      await assertHandoffReadinessRetries(origin, expectedRevision, deploymentToken, evidence,
        source + "-readiness-control-test", async function () {
          gateway.vars.REVIEW_HANDOFF_REVISION = expectedRevision;
          await writeGatewayConfiguration(gatewayPath, gateway);
        });
      await assertHealthy(origin, expectedRevision, failureEvidence);
      const settledDeadline = Date.now() + 120000;
      while (!stop.readOutput().split("\n").some(function (line) {
        return line.includes("webhook review settled") && line.includes(deliveryId);
      }) && Date.now() < settledDeadline) {
        await new Promise(function (resolve) { setTimeout(resolve, 250); });
      }
      assert.ok(stop.readOutput().split("\n").some(function (line) {
        return line.includes("webhook review settled") && line.includes(deliveryId);
      }), "retained delivery did not complete the interrupted handoff without controller advance");
      const complete = await stateWhen(expectedRevision, function (state) { return state.complete; });
      assert.equal(complete.candidate_revision, expectedRevision);
      assert.equal(complete.previous_container_running, false);
      gateway.vars.REVIEW_HANDOFF = "false";
      await writeGatewayConfiguration(gatewayPath, gateway);
      await stateWhen(expectedRevision, function (state) { return !state.handoff; });
      const delayed = await fetch(origin + "/");
      assert.equal(delayed.status, 503, "delayed source fetch bypassed the persisted handoff fence");
      await delayed.body?.cancel();
      const fenced = await stateWhen(expectedRevision, function (state) { return !state.handoff; });
      assert.equal(fenced.previous_container_running, false, "delayed source fetch restarted the stopped source");
      gateway.vars.REVIEW_HANDOFF = "true";
      await writeGatewayConfiguration(gatewayPath, gateway);
      await assertHealthy(origin, expectedRevision, failureEvidence);
      await stateWhen(expectedRevision, function (state) { return state.complete; });
      proof.phases.push({ mode: source + "-handoff", revision: expectedRevision, readinessRetried: true,
        replaySettled: true, sourceFenced: true });
    } finally {
      await stop();
    }
  }
  await writeFile(path.join(evidence, "proof.json"), JSON.stringify(proof, null, 2), { mode: 0o600 });
});
