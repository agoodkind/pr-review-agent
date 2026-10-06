import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHash, createHmac } from "node:crypto";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const signingKey = "test-review-budget-secret";
const cloudflareDirectory = fileURLToPath(new URL("..", import.meta.url));
const providerId = "provider_a";
const otherProviderId = "provider_b";
const modelId = "model_a";
const otherModelId = "model_b";

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
  const operatorDigest = createHash("sha256").update("test-operator-token").digest("hex");
  const child = spawn(wrangler, [
    "dev", "--config", "test/budget.wrangler.jsonc", "--local",
    "--port", String(port), "--inspector-port", "0", "--persist-to", persistPath,
    "--var", `OPERATOR_TOKEN_SHA256:${operatorDigest}`,
  ], { cwd: cloudflareDirectory, stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  try {
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
      child.once("error", function (error) {
        clearTimeout(timeout);
        reject(error);
      });
      child.once("exit", function (code) {
        clearTimeout(timeout);
        reject(new Error("budget worker exited " + code + ": " + output));
      });
    });
  } catch (error) {
    await stopRuntime(child);
    throw error;
  }
  return child;
}

async function stopRuntime(child) {
  if (!child || !child.pid || child.exitCode !== null || child.signalCode !== null) {
    return;
  }
  await new Promise(function (resolve) {
    child.once("exit", resolve);
    child.kill("SIGINT");
  });
}

async function budgetRequest(port, payload, expectedStatus = 200) {
  const body = JSON.stringify(payload);
  const signature = "sha256=" + createHmac("sha256", signingKey).update(body).digest("hex");
  const response = await fetch(`http://127.0.0.1:${port}/internal/v1/provider_budget`, {
    method: "POST",
    headers: { "X-Pr-Agent-Budget-Signature": signature },
    body,
  });
  if (expectedStatus !== 200) {
    assert.equal(response.status, expectedStatus, await response.text());
    return;
  }
  assert.equal(response.status, 200, response.status === 200 ? undefined : await response.text());
  return response.json();
}

function check(port, providerId, model = modelId) {
  return budgetRequest(port, { provider_id: providerId, model, limit: 2_000_000 });
}

function report(port, providerId, day, tokens, model = modelId) {
  return budgetRequest(port, { provider_id: providerId, model, day, tokens });
}

function queryUsage(port, providerId, start, end, options = {}) {
  return budgetRequest(port, {
    action: "query_usage", provider_id: providerId, model: modelId,
    start_ms: start, end_ms: end, include_start: true, include_end: true,
    after_sequence: 0, snapshot_sequence: 0, page_size: 500, ...options,
  });
}

function reportUsage(port, providerId, admittedAt, options = {}) {
  return budgetRequest(port, {
    action: "report_usage", provider_id: providerId, model: modelId,
    admitted_at_ms: admittedAt, input_tokens: 11, output_tokens: 7, total_tokens: 20,
    ...options,
  });
}

async function operatorRequest(port, body, expectedStatus = 200, authorization = "Bearer test-operator-token") {
  const response = await fetch(`http://127.0.0.1:${port}/internal/v1/provider_budget`, {
    method: "POST", headers: { Authorization: authorization }, body,
  });
  assert.equal(response.status, expectedStatus, response.ok ? undefined : await response.text());
  if (response.ok) {
    return response.json();
  }
}

test("operator authentication reads actual counters and rejects usage writes", async function () {
  const persistPath = await mkdtemp(path.join(os.tmpdir(), "pr-agent-operator-"));
  const port = await unusedPort();
  const admittedAt = Date.now() - 1000;
  let runtime;
  try {
    runtime = await startRuntime(port, persistPath);
    const daily = await check(port, providerId);
    await reportUsage(port, providerId, admittedAt, { legacy_day: daily.day, legacy_tokens: 18 });
    const snapshot = { provider_id: providerId, model: modelId, limit: 2_000_000 };
    const query = {
      action: "query_usage", provider_id: providerId, model: modelId,
      start_ms: 0, end_ms: Date.now(), include_start: true, include_end: true,
      after_sequence: 0, snapshot_sequence: 0, page_size: 500,
    };
    assert.deepEqual(await operatorRequest(port, JSON.stringify(snapshot)), await check(port, providerId));
    const before = await operatorRequest(port, JSON.stringify(query));
    assert.equal(before.events.length, 1);
    assert.deepEqual([before.events[0].input_tokens, before.events[0].output_tokens], [11, 7]);
    for (const authorization of ["", "Bearer wrong-token", "Basic test-operator-token", "Bearer test-operator-token extra"]) {
      await operatorRequest(port, JSON.stringify(snapshot), 401, authorization);
    }
    await operatorRequest(port, "null", 400);
    await operatorRequest(port, "[]", 400);
    await operatorRequest(port, "{", 400);
    for (const mutation of [
      { ...snapshot, tokens: 1 },
      { provider_id: providerId, model: modelId, day: daily.day, tokens: 1 },
      { ...query, action: "report_usage", admitted_at_ms: admittedAt, input_tokens: 1, output_tokens: 0, total_tokens: 1 },
      { ...query, tokens: 1 },
      { ...query, action: "reserve" },
      { ...snapshot, constructor: { tokens: 1 } },
      { ...snapshot, prototype: { tokens: 1 } },
    ]) {
      await operatorRequest(port, JSON.stringify(mutation), 403);
    }
    await operatorRequest(port, '{"provider_id":"provider_a","model":"model_a","limit":2000000,"__proto__":{"tokens":1}}', 403);
    assert.equal((await check(port, providerId)).used, 18);
    assert.deepEqual(await operatorRequest(port, JSON.stringify(query)), before);
    await report(port, providerId, daily.day, 1);
    assert.equal((await operatorRequest(port, JSON.stringify(snapshot))).used, 19);
  } finally {
    await stopRuntime(runtime);
    await rm(persistPath, { recursive: true, force: true });
  }
});

test("reported tokens set the daily provider limit and survive worker restart", async function () {
  const persistPath = await mkdtemp(path.join(os.tmpdir(), "pr-agent-budget-"));
  const port = await unusedPort();
  let runtime;
  try {
    runtime = await startRuntime(port, persistPath);
    const first = await check(port, providerId);
    assert.equal(first.allowed, true);
    assert.deepEqual([first.used, first.limit, first.remaining], [0, 2_000_000, 2_000_000]);
    assert.match(first.day, /^\d{4}-\d{2}-\d{2}$/);
    assert.deepEqual(await report(port, providerId, first.day, 1_500_000), { reported: true });
    assert.equal((await check(port, providerId)).allowed, true);
    assert.deepEqual(await report(port, providerId, first.day, 500_000), { reported: true });
    assert.equal((await check(port, otherProviderId)).allowed, true);
    assert.deepEqual(await check(port, providerId, otherModelId), {
      allowed: true, day: first.day, used: 0, limit: 2_000_000, remaining: 2_000_000,
    });
    assert.deepEqual(await budgetRequest(port, {
      provider_id: "legacy", tokens: 2_000_000, limit: 2_000_000,
    }), { allowed: true });

    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath);
    assert.deepEqual(await check(port, providerId), {
      allowed: false, day: first.day, used: 2_000_000, limit: 2_000_000, remaining: 0,
    });
    assert.equal((await check(port, providerId, otherModelId)).remaining, 2_000_000);
  } finally {
    await stopRuntime(runtime);
    await rm(persistPath, { recursive: true, force: true });
  }
});

test("timestamped usage preserves daily accounting and stable range snapshots", async function (context) {
  const persistPath = await mkdtemp(path.join(os.tmpdir(), "pr-agent-usage-"));
  const port = await unusedPort();
  const admittedAt = Date.now() - 10_000;
  let runtime;
  try {
    runtime = await startRuntime(port, persistPath);
    await context.test("legacy totals do not create timestamped usage", async function () {
      const daily = await check(port, providerId);
      await report(port, providerId, daily.day, 500);
      const before = Date.now();
      const empty = await queryUsage(port, providerId, 0, Date.now());
      assert.deepEqual(empty.events, []);
      assert.equal(empty.snapshot_sequence, 0);
      assert.equal(empty.next_sequence, 0);
      assert.ok(empty.history_start_ms >= before && empty.history_start_ms <= Date.now());
      await reportUsage(port, providerId, admittedAt, {
        legacy_day: daily.day, legacy_tokens: 11, tokens: -1, limit: -1,
      });
      await reportUsage(port, providerId, admittedAt + 1, {
        legacy_day: daily.day, legacy_tokens: 0,
      });
      assert.equal((await check(port, providerId)).used, 511);
      const typed = await queryUsage(port, providerId, 0, Date.now());
      assert.equal(typed.history_start_ms, empty.history_start_ms);
      assert.deepEqual(typed.events.map(function (event) {
        return [event.admitted_at_ms, event.input_tokens, event.output_tokens, event.total_tokens];
      }), [[admittedAt, 11, 7, 20], [admittedAt + 1, 11, 7, 20]]);
    });
    await context.test("each interval edge uses its requested inclusion", async function () {
      for (const offset of [100, 200, 300]) {
        await reportUsage(port, "ranges", admittedAt + offset);
      }
      for (const [includeStart, includeEnd, offsets] of [
        [true, true, [100, 200, 300]], [false, true, [200, 300]],
        [true, false, [100, 200]], [false, false, [200]],
      ]) {
        const result = await queryUsage(port, "ranges", admittedAt + 100, admittedAt + 300, {
          include_start: includeStart, include_end: includeEnd,
        });
        assert.deepEqual(result.events.map(function (event) { return event.admitted_at_ms; }),
          offsets.map(function (offset) { return admittedAt + offset; }));
      }
      assert.equal((await queryUsage(port, "ranges", admittedAt + 100, admittedAt + 100)).events.length, 1);
      assert.equal((await queryUsage(port, "ranges", admittedAt + 100, admittedAt + 100, {
        include_start: false,
      })).events.length, 0);
    });
    await context.test("pagination excludes inserts after the initial snapshot", async function () {
      for (let index = 0; index < 7; index += 1) {
        await reportUsage(port, "pages", admittedAt + index);
      }
      const first = await queryUsage(port, "pages", 0, Date.now(), { page_size: 2 });
      assert.equal(first.events.length, 2);
      assert.equal(first.next_sequence, first.events.at(-1).sequence);
      await Promise.all([
        reportUsage(port, "pages", admittedAt),
        reportUsage(port, otherProviderId, admittedAt),
        reportUsage(port, "pages", admittedAt, { model: otherModelId }),
      ]);
      const events = [...first.events];
      let next = first.next_sequence;
      while (next !== 0) {
        const page = await queryUsage(port, "pages", 0, Date.now(), {
          page_size: 2, after_sequence: next, snapshot_sequence: first.snapshot_sequence,
        });
        assert.equal(page.snapshot_sequence, first.snapshot_sequence);
        events.push(...page.events);
        next = page.next_sequence;
      }
      assert.equal(events.length, 7);
      assert.equal(new Set(events.map(function (event) { return event.sequence; })).size, 7);
      assert.equal((await queryUsage(port, "pages", 0, Date.now())).events.length, 8);
      assert.equal((await queryUsage(port, "pages", 0, Date.now(), { model: otherModelId })).events.length, 1);
      assert.equal((await queryUsage(port, otherProviderId, 0, Date.now())).events.length, 1);
      assert.equal((await check(port, "pages")).used, 0);
    });
    await context.test("model reports preserve configured provider-only migration", async function () {
      const configuration = JSON.parse(await readFile(new URL("../../../runtime.json", import.meta.url), "utf8"));
      const configuredProvider = configuration.PROVIDERS[0];
      const daily = await budgetRequest(port, { provider_id: configuredProvider.id, limit: 2_000_000 });
      await budgetRequest(port, { provider_id: configuredProvider.id, day: daily.day, tokens: 100 });
      await reportUsage(port, configuredProvider.id, admittedAt, {
        model: configuredProvider.model, legacy_day: daily.day, legacy_tokens: 11,
      });
      assert.equal((await check(port, configuredProvider.id, configuredProvider.model)).used, 111);
      assert.equal((await budgetRequest(port, { provider_id: configuredProvider.id, limit: 2_000_000 })).used, 0);
      assert.equal((await queryUsage(port, configuredProvider.id, 0, Date.now(), {
        model: configuredProvider.model,
      })).events.length, 1);
    });
    await context.test("invalid usage reports and queries return client errors", async function () {
      const validReport = {
        action: "report_usage", provider_id: "invalid", model: modelId,
        admitted_at_ms: admittedAt, input_tokens: 1, output_tokens: 2, total_tokens: 3,
      };
      for (const invalidFields of [
        { admitted_at_ms: -1 }, { admitted_at_ms: 0.5 }, { admitted_at_ms: Number.MAX_SAFE_INTEGER + 1 },
        { input_tokens: -1 }, { output_tokens: "2" }, { total_tokens: Number.MAX_SAFE_INTEGER + 1 },
        { model: "" }, { model: null }, { provider_id: "Invalid" }, { legacy_day: "2026-10-04" },
        { legacy_tokens: 1 }, { legacy_day: "bad", legacy_tokens: 1 },
        { legacy_day: "2026-10-04", legacy_tokens: -1 },
      ]) {
        await budgetRequest(port, { ...validReport, ...invalidFields }, 400);
      }
      const validQuery = {
        action: "query_usage", provider_id: "invalid", model: modelId,
        start_ms: 0, end_ms: Date.now(), include_start: true, include_end: false,
        after_sequence: 0, snapshot_sequence: 0, page_size: 500,
      };
      for (const invalidFields of [
        { start_ms: -1 }, { start_ms: Number.MAX_SAFE_INTEGER + 1 }, { end_ms: 0.5 },
        { start_ms: validQuery.end_ms + 1 }, { include_start: 1 }, { include_end: null },
        { after_sequence: -1 }, { after_sequence: 1 }, { snapshot_sequence: 0.5 },
        { snapshot_sequence: Number.MAX_SAFE_INTEGER }, { page_size: 501 }, { page_size: 0 },
      ]) {
        await budgetRequest(port, { ...validQuery, ...invalidFields }, 400);
      }
      await budgetRequest(port, { ...validQuery, action: "unknown", tokens: 1 }, 400);
      await budgetRequest(port, null, 400);
      assert.deepEqual((await queryUsage(port, "invalid", 0, Date.now())).events, []);
    });
    await context.test("daily counter overflow rejects the complete usage report", async function () {
      const daily = await check(port, "overflow");
      await report(port, "overflow", daily.day, Number.MAX_SAFE_INTEGER);
      await budgetRequest(port, {
        action: "report_usage", provider_id: "overflow", model: modelId,
        admitted_at_ms: admittedAt, input_tokens: 1, output_tokens: 0, total_tokens: 1,
        legacy_day: daily.day, legacy_tokens: 1,
      }, 400);
      assert.equal((await check(port, "overflow")).used, Number.MAX_SAFE_INTEGER);
      assert.deepEqual((await queryUsage(port, "overflow", 0, Date.now())).events, []);
    });
    const persisted = await queryUsage(port, providerId, 0, Date.now());
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath);
    assert.deepEqual(await queryUsage(port, providerId, 0, Date.now()), persisted);
    assert.equal((await check(port, providerId)).used, 511);
  } finally {
    await stopRuntime(runtime);
    await rm(persistPath, { recursive: true, force: true });
  }
});
