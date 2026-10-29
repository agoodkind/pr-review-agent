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

async function startRuntime(port, persistPath, retention, totalsWidth) {
  const wrangler = path.join(cloudflareDirectory, "node_modules/.bin/wrangler");
  const operatorDigest = createHash("sha256").update("test-operator-token").digest("hex");
  const arguments_ = [
    "dev", "--config", "test/budget.wrangler.jsonc", "--local",
    "--port", String(port), "--inspector-port", "0", "--persist-to", persistPath,
    "--var", `OPERATOR_TOKEN_SHA256:${operatorDigest}`,
  ];
  if (retention !== undefined) {
    arguments_.push("--var", `PROVIDER_USAGE_RETENTION:${JSON.stringify(retention)}`);
  }
  if (totalsWidth !== undefined) {
    arguments_.push("--var", `PROVIDER_USAGE_TOTAL_BUCKET_WIDTH:${totalsWidth}`);
  }
  const child = spawn(wrangler, arguments_, { cwd: cloudflareDirectory, stdio: ["ignore", "pipe", "pipe"] });
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

function queryTotals(port, providerId, start, end, options = {}) {
  return budgetRequest(port, {
    action: "query_usage_totals", provider_id: providerId, model: modelId,
    start_ms: start, end_ms: end, include_start: true, include_end: true,
    after_group: 0, snapshot_sequence: 0, page_size: 500, ...options,
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

test("retention preserves disabled history and removes expired reports without permitting replay", async function () {
  const persistPath = await mkdtemp(path.join(os.tmpdir(), "pr-agent-retention-"));
  const port = await unusedPort();
  const oldAdmission = Date.now() - 72 * 3600000;
  const recentAdmission = Date.now() - 3600000;
  const sdk = { api: "responses", usage_json: '{"input_tokens":11,"future":null}' };
  const disabled = { max_age: "0s", cleanup_interval: "100ms", batch_size: 1 };
  const stats = () => operatorRequest(port, JSON.stringify({ action: "storage_stats" }));
  async function until(predicate) {
    const deadline = Date.now() + 10000;
    while (Date.now() < deadline) {
      const result = await stats();
      if (predicate(result)) {
        return result;
      }
      await new Promise(function (resolve) { setTimeout(resolve, 50); });
    }
    assert.fail("usage maintenance did not complete");
  }
  let runtime;
  try {
    runtime = await startRuntime(port, persistPath, disabled);
    const daily = await check(port, providerId);
    const oldDay = new Date(oldAdmission).toISOString().slice(0, 10);
    await reportUsage(port, providerId, oldAdmission, { report_id: "old-sdk", sdk_usage: sdk,
      legacy_day: oldDay, legacy_tokens: 18 });
    await reportUsage(port, providerId, oldAdmission + 1, { report_id: "old-second" });
    await reportUsage(port, providerId, recentAdmission, { report_id: "retained-sdk", sdk_usage: sdk,
      legacy_day: daily.day, legacy_tokens: 18 });
    const before = await until(function (result) { return result.retention.sdk_migration_complete && result.retention.next_maintenance_ms === null; });
    assert.equal(before.events, 3);
    assert.equal(before.sdk_records, 2);
    assert.equal(before.retention.cutoff_ms, 0);
    assert.ok(before.database_bytes > 0);
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath, { ...disabled, max_age: "1h" });
    const after = await until(function (result) { return result.events === 1 && result.sdk_records === 1; });
    assert.equal(after.details, 1);
    assert.equal(after.sdk_payload_bytes, Buffer.byteLength(JSON.stringify(sdk)));
    assert.equal(after.retention.configured_max_age_ms, 3600000);
    assert.equal(after.retention.effective_max_age_ms, 86400000);
    assert.equal(after.retention.enabled, true);
    assert.ok(after.retention.cutoff_ms > oldAdmission && after.retention.cutoff_ms < recentAdmission);
    const retained = await queryUsage(port, providerId, 0, Date.now());
    assert.equal(retained.events.length, 1);
    assert.deepEqual(retained.events[0].sdk_usage, sdk);
    assert.ok(retained.history_start_ms >= after.retention.cutoff_ms);
    const expired = { action: "report_usage", provider_id: providerId, model: modelId,
      admitted_at_ms: oldAdmission, input_tokens: 11, output_tokens: 7, total_tokens: 20,
      report_id: "old-sdk", sdk_usage: sdk, legacy_day: oldDay, legacy_tokens: 18 };
    await budgetRequest(port, expired, 410);
    await reportUsage(port, providerId, recentAdmission, { report_id: "retained-sdk", sdk_usage: sdk,
      legacy_day: daily.day, legacy_tokens: 18 });
    assert.equal((await check(port, providerId)).used, 18);
    await budgetRequest(port, { provider_id: providerId, model: modelId, day: oldDay, tokens: 18 }, 410);
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath, disabled);
    await budgetRequest(port, expired, 410);
    assert.equal((await queryUsage(port, providerId, 0, Date.now())).events.length, 1);
    await operatorRequest(port, JSON.stringify({ action: "storage_stats", tokens: 1 }), 403);
  } finally {
    await stopRuntime(runtime);
    await rm(persistPath, { recursive: true, force: true });
  }
});

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
    await context.test("usage details persist without inventing historical counters", async function () {
      const report = { report_id: "billing-details", cached_input_tokens: 4,
        cache_write_tokens: 3, service_tier: "default" };
      await reportUsage(port, "billing", admittedAt, report);
      await reportUsage(port, "billing", admittedAt, report);
      const stored = await queryUsage(port, "billing", 0, Date.now());
      assert.equal(stored.events.length, 1);
      assert.deepEqual([stored.events[0].cached_input_tokens, stored.events[0].cache_write_tokens,
        stored.events[0].service_tier], [4, 3, "default"]);
      await budgetRequest(port, { action: "report_usage", provider_id: "billing", model: modelId,
        admitted_at_ms: admittedAt, input_tokens: 11, output_tokens: 7, total_tokens: 20,
        ...report, cache_write_tokens: 2 }, 409);
      const legacy = await queryUsage(port, providerId, 0, Date.now());
      assert.equal(legacy.events[0].cache_write_tokens, null);
      assert.equal(legacy.events[0].cached_input_tokens, null);
      assert.equal(legacy.events[0].service_tier, null);
      assert.equal(legacy.events[0].sdk_usage, null);
      await stopRuntime(runtime);
      runtime = await startRuntime(port, persistPath);
      assert.deepEqual(await queryUsage(port, "billing", 0, Date.now()), stored);
    });
    await context.test("native SDK usage remains exact across byte-bounded history pages", async function () {
      const configuration = JSON.parse(await readFile(new URL("../../../runtime.json", import.meta.url), "utf8"));
      const pageBytes = Number(configuration.PROVIDER_USAGE_PAGE_BYTES);
      const reportBytes = Number(configuration.PROVIDER_USAGE_MAX_BYTES);
      const native = { api: "responses", model: "selected_model", response_id: "response_identity", future_envelope: { revision: 1 },
        usage_json: `{ "future_count": 123456789012345678901234567890, "future": { "explicit_null": null, "items": [true, "${"界".repeat(3000)}"] } }` };
      const details = { report_id: "native-envelope", sdk_usage: native };
      assert.deepEqual(await reportUsage(port, "native", admittedAt, details), { reported: true, sdk_usage_stored: true });
      assert.deepEqual(await reportUsage(port, "native", admittedAt, details), { reported: true, sdk_usage_stored: true });
      const one = await queryUsage(port, "native", 0, Date.now());
      assert.equal(one.events.length, 1);
      assert.deepEqual(one.events[0].sdk_usage, native);
      for (const changed of [
        { ...native, api: "chat_completions" }, { ...native, model: "another_model" },
        { ...native, response_id: "another_response" }, { ...native, usage_json: "{}" },
        { ...native, future_envelope: { revision: 2 } },
      ]) {
        await budgetRequest(port, { action: "report_usage", provider_id: "native", model: modelId,
          admitted_at_ms: admittedAt, input_tokens: 11, output_tokens: 7, total_tokens: 20,
          ...details, sdk_usage: changed }, 409);
      }
      for (let index = 1; index < 20; index += 1) {
        await reportUsage(port, "native", admittedAt + index, { sdk_usage: native });
      }
      const first = await queryUsage(port, "native", 0, Date.now());
      assert.ok(first.next_sequence > 0);
      assert.ok(first.events.length < 20);
      assert.ok(Buffer.byteLength(JSON.stringify(first)) <= pageBytes);
      await reportUsage(port, "native", admittedAt, { sdk_usage: native });
      const events = [...first.events];
      let next = first.next_sequence;
      while (next !== 0) {
        const page = await queryUsage(port, "native", 0, Date.now(), {
          after_sequence: next, snapshot_sequence: first.snapshot_sequence,
        });
        assert.equal(page.snapshot_sequence, first.snapshot_sequence);
        assert.ok(Buffer.byteLength(JSON.stringify(page)) <= pageBytes);
        events.push(...page.events);
        next = page.next_sequence;
      }
      assert.equal(events.length, 20);
      assert.equal(new Set(events.map(function (event) { return event.sequence; })).size, 20);
      for (const event of events) {
        assert.deepEqual(event.sdk_usage, native);
        assert.deepEqual([event.input_tokens, event.output_tokens, event.total_tokens], [11, 7, 20]);
      }
      assert.deepEqual(await reportUsage(port, "nullable_sdk", admittedAt, { report_id: "legacy-envelope" }), { reported: true, sdk_usage_stored: false });
      assert.deepEqual(await reportUsage(port, "nullable_sdk", admittedAt, { report_id: "legacy-envelope", sdk_usage: null }), { reported: true, sdk_usage_stored: false });
      assert.equal((await queryUsage(port, "nullable_sdk", 0, Date.now())).events.length, 1);
      await budgetRequest(port, { action: "report_usage", provider_id: "too_large", model: modelId,
        admitted_at_ms: admittedAt, input_tokens: 1, output_tokens: 1, total_tokens: 2,
        sdk_usage: { api: "responses", usage_json: JSON.stringify({ future: "x".repeat(reportBytes) }) } }, 413);
      const persisted = await queryUsage(port, "native", 0, Date.now(), { page_size: 1 });
      await stopRuntime(runtime);
      runtime = await startRuntime(port, persistPath);
      assert.deepEqual(await queryUsage(port, "native", 0, Date.now(), { page_size: 1 }), persisted);
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
        { report_id: "" }, { report_id: null }, { report_id: "bad/identity" },
        { cached_input_tokens: -1 }, { cache_write_tokens: 2 },
        { cached_input_tokens: 1, cache_write_tokens: 1 }, { service_tier: 42 },
        { sdk_usage: [] }, { sdk_usage: {} },
        { sdk_usage: { api: " ", usage_json: "{}" } },
        { sdk_usage: { api: "responses", usage_json: "[]" } },
        { sdk_usage: { api: "responses", usage_json: "null" } },
        { sdk_usage: { api: "responses", usage_json: "{" } },
        { sdk_usage: { api: "responses", usage_json: "{}", model: 1 } },
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
    const replayedReport = {
      report_id: "concurrent-replay", legacy_day: new Date().toISOString().slice(0, 10),
      legacy_tokens: 18,
    };
    await Promise.all(Array.from({ length: 5 }, function () {
      return reportUsage(port, "replayed", admittedAt, replayedReport);
    }));
    const persisted = await queryUsage(port, providerId, 0, Date.now());
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath);
    await reportUsage(port, "replayed", admittedAt, replayedReport);
    assert.equal((await queryUsage(port, "replayed", 0, Date.now())).events.length, 1);
    assert.equal((await check(port, "replayed")).used, 18);
    assert.deepEqual(await queryUsage(port, providerId, 0, Date.now()), persisted);
    assert.equal((await check(port, providerId)).used, 511);
  } finally {
    await stopRuntime(runtime);
    await rm(persistPath, { recursive: true, force: true });
  }
});

test("saved totals preserve raw records, delayed admissions, snapshots, and width changes", async function () {
  const persistPath = await mkdtemp(path.join(os.tmpdir(), "pr-agent-saved-totals-"));
  const port = await unusedPort();
  const hour = 3600000;
  const start = Math.floor((Date.now() - 48 * hour) / hour) * hour;
  const end = start + 4 * hour;
  const disabled = { max_age: "0s", cleanup_interval: "100ms", batch_size: 1 };
  const native = { api: "responses", model: "reported_model", response_id: "stored-response",
    usage_json: '{"future_count":123456789012345678901234567890,"future_data":[null,true]}' };
  const details = { cached_input_tokens: 4, cache_write_tokens: 3, service_tier: "default", sdk_usage: native };
  const stats = () => operatorRequest(port, JSON.stringify({ action: "storage_stats" }));
  let runtime;
  try {
    runtime = await startRuntime(port, persistPath, disabled, "1h");
    for (const [index, admission] of [start + 1000, start + 2000, start + 3 * hour + 1000].entries()) {
      await reportUsage(port, "saved", admission, { ...details, report_id: `saved-${index}` });
    }
    const original = await queryUsage(port, "saved", start, end, { include_end: false });
    assert.equal((await stats()).totals_buckets, 0);
    const initial = await queryTotals(port, "saved", start, end, { include_end: false });
    assert.equal(initial.groups.length, 1);
    assert.deepEqual([initial.groups[0].requests, initial.groups[0].total_tokens], [3, 60]);
    assert.equal(initial.groups[0].first_admitted_at_ms, start + 1000);
    assert.deepEqual([(await stats()).totals_buckets, (await stats()).totals_groups], [2, 2]);
    assert.deepEqual(await queryUsage(port, "saved", start, end, { include_end: false }), original);

    const late = { ...details, report_id: "saved-late", sdk_usage: { ...native, model: "late_model" } };
    await reportUsage(port, "saved", start + 500, late);
    assert.deepEqual([(await stats()).totals_buckets, (await stats()).totals_groups], [2, 3]);
    await reportUsage(port, "saved", start + 400, { ...details, report_id: "saved-late-existing" });
    assert.deepEqual([(await stats()).totals_buckets, (await stats()).totals_groups], [2, 3]);
    const updated = await queryTotals(port, "saved", start, end, { include_end: false });
    assert.equal(updated.groups[0].reported_model, "late_model");
    assert.equal(updated.groups[0].first_admitted_at_ms, start + 500);
    assert.equal(updated.groups[1].first_admitted_at_ms, start + 400);
    assert.equal(updated.groups[1].requests, 4);
    assert.equal(updated.groups.reduce(function (count, group) { return count + group.requests; }, 0), 5);
    const historical = await queryTotals(port, "saved", start, end, {
      include_end: false, snapshot_sequence: initial.snapshot_sequence,
      retention_generation: initial.retention_generation,
    });
    assert.deepEqual(historical.groups, initial.groups);
    await reportUsage(port, "saved", start + 500, late);
    assert.deepEqual((await queryTotals(port, "saved", start, end, { include_end: false })).groups, updated.groups);
    await budgetRequest(port, { action: "report_usage", provider_id: "saved", model: modelId,
      admitted_at_ms: start + 500, input_tokens: 11, output_tokens: 7, total_tokens: 21, ...late }, 409);
    for (const [left, right, includeStart, includeEnd] of [
      [start + 750, start + 2000, true, false],
      [start + 500, start + 2000, false, true],
      [start + 500, end, true, false],
    ]) {
      const selected = await queryUsage(port, "saved", left, right, { include_start: includeStart, include_end: includeEnd });
      const grouped = await queryTotals(port, "saved", left, right, { include_start: includeStart, include_end: includeEnd });
      assert.equal(grouped.groups.reduce(function (count, group) { return count + group.requests; }, 0), selected.events.length);
      assert.equal(grouped.groups.reduce(function (count, group) { return count + group.total_tokens; }, 0),
        selected.events.reduce(function (count, event) { return count + event.total_tokens; }, 0));
    }

    const beforeRestart = await queryUsage(port, "saved", start, end);
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath, disabled, "2h");
    const resized = await queryTotals(port, "saved", start, end, { include_end: false });
    assert.deepEqual(resized.groups, updated.groups);
    const completeStarts = new Set([start + 1000, start + 2000, start + 3 * hour + 1000, start + 500]
      .map(function (admission) { return Math.floor(admission / (2 * hour)) * (2 * hour); })
      .filter(function (bucket) { return bucket >= start && bucket + 2 * hour <= end; }));
    assert.equal((await stats()).totals_buckets, completeStarts.size);
    const savedCount = (await stats()).totals_buckets;
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath, disabled, "2h");
    assert.equal((await stats()).totals_buckets, savedCount);
    assert.deepEqual(await queryUsage(port, "saved", start, end), beforeRestart);

    const overflowStart = Math.floor(start / (2 * hour)) * (2 * hour);
    const overflowEnd = overflowStart + 2 * hour;
    const first = { report_id: "cache-overflow-first", input_tokens: 0, output_tokens: 0,
      total_tokens: Number.MAX_SAFE_INTEGER, sdk_usage: native };
    await reportUsage(port, "cache_overflow", start + 1000, first);
    const overflowSnapshot = await queryTotals(port, "cache_overflow", overflowStart, overflowEnd, { include_end: false });
    const beforeFailure = await stats();
    const second = { ...first, report_id: "cache-overflow-second", total_tokens: 1 };
    assert.deepEqual(await reportUsage(port, "cache_overflow", start + 1000, second), { reported: true, sdk_usage_stored: true });
    const afterFailure = await stats();
    assert.equal(afterFailure.events, beforeFailure.events + 1);
    assert.equal(afterFailure.sdk_records, beforeFailure.sdk_records + 1);
    assert.equal(afterFailure.totals_buckets, beforeFailure.totals_buckets - 1);
    const retained = await queryUsage(port, "cache_overflow", overflowStart, overflowEnd);
    assert.deepEqual(retained.events.map(function (event) { return event.total_tokens; }), [Number.MAX_SAFE_INTEGER, 1]);
    assert.ok(retained.events.every(function (event) { return JSON.stringify(event.sdk_usage) === JSON.stringify(native); }));
    await budgetRequest(port, { action: "query_usage_totals", provider_id: "cache_overflow", model: modelId,
      start_ms: overflowStart, end_ms: overflowEnd, include_start: true, include_end: false,
      after_group: 0, snapshot_sequence: 0, page_size: 500 }, 400);
    const beforeOverflow = await queryTotals(port, "cache_overflow", overflowStart, overflowEnd, {
      include_end: false, snapshot_sequence: overflowSnapshot.snapshot_sequence,
      retention_generation: overflowSnapshot.retention_generation,
    });
    assert.equal(beforeOverflow.groups[0].requests, 1);
    assert.equal(beforeOverflow.groups[0].total_tokens, Number.MAX_SAFE_INTEGER);
    const future = Date.now() + 48 * hour;
    const futureStart = Math.floor(future / (2 * hour)) * (2 * hour);
    await reportUsage(port, "future_saved", future, { ...details, report_id: "future-saved" });
    assert.equal((await queryTotals(port, "future_saved", futureStart, futureStart + 2 * hour, { include_end: false })).groups[0].requests, 1);
    assert.deepEqual((await queryTotals(port, "future_saved", 0, Date.now())).groups, []);
    assert.deepEqual((await queryTotals(port, "future_saved", 0, future - 1)).groups, []);
    assert.equal((await queryTotals(port, "future_saved", 0, future)).groups[0].requests, 1);
    assert.equal((await check(port, "saved")).used, 0);
  } finally {
    await stopRuntime(runtime);
    await rm(persistPath, { recursive: true, force: true });
  }
});

test("pruning invalidates saved totals and rejects paging drift at an unchanged cutoff", async function () {
  const persistPath = await mkdtemp(path.join(os.tmpdir(), "pr-agent-total-retention-"));
  const port = await unusedPort();
  const oldAdmission = Date.now() - 96 * 3600000;
  const recentAdmission = Date.now() - 1000;
  const disabled = { max_age: "0s", cleanup_interval: "100ms", batch_size: 1 };
  const native = { api: "responses", model: "retained-model", usage_json: '{"future":null}' };
  const stats = () => operatorRequest(port, JSON.stringify({ action: "storage_stats" }));
  async function until(predicate) {
    const deadline = Date.now() + 10000;
    while (Date.now() < deadline) {
      const snapshot = await stats();
      if (predicate(snapshot)) {
        return snapshot;
      }
      await new Promise(function (resolve) { setTimeout(resolve, 10); });
    }
    assert.fail("The expected real retention batch did not complete.");
  }
  let runtime;
  try {
    runtime = await startRuntime(port, persistPath, disabled, "1h");
    for (let index = 0; index < 20; index += 1) {
      await reportUsage(port, "pruned_totals", oldAdmission + index, {
        report_id: `old-${index}`, sdk_usage: { ...native, model: `old-model-${index}` },
      });
    }
    await reportUsage(port, "pruned_totals", recentAdmission, { report_id: "recent", sdk_usage: native });
    await queryTotals(port, "pruned_totals", 0, Date.now());
    assert.ok((await stats()).totals_buckets > 0);
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath, { ...disabled, max_age: "1h" }, "1h");
    const firstPrune = await until(function (snapshot) { return snapshot.events < 21; });
    assert.equal(firstPrune.totals_buckets, 0);
    await stopRuntime(runtime);
    runtime = await startRuntime(port, persistPath, { ...disabled, max_age: "72h" }, "1h");
    const before = await stats();
    const end = Date.now();
    const firstPage = await queryTotals(port, "pruned_totals", 0, end, { page_size: 1 });
    assert.ok(firstPage.next_group > 0);
    assert.ok(firstPage.retention_generation > 0);
    const nextPrune = await until(function (snapshot) { return snapshot.events < before.events; });
    assert.equal(nextPrune.retention.cutoff_ms, firstPrune.retention.cutoff_ms);
    await operatorRequest(port, JSON.stringify({ action: "query_usage_totals", provider_id: "pruned_totals", model: modelId,
      start_ms: 0, end_ms: end, include_start: true, include_end: true,
      after_group: firstPage.next_group, snapshot_sequence: firstPage.snapshot_sequence,
      retention_generation: firstPage.retention_generation, page_size: 1 }), 409);
    await until(function (snapshot) { return snapshot.events === 1; });
    const result = await queryTotals(port, "pruned_totals", 0, Date.now());
    assert.deepEqual([result.groups.length, result.groups[0].requests, result.groups[0].total_tokens], [1, 1, 20]);
    const history = await queryUsage(port, "pruned_totals", 0, Date.now());
    assert.equal(history.events.length, 1);
    assert.deepEqual(history.events[0].sdk_usage, native);
    await budgetRequest(port, { action: "report_usage", provider_id: "pruned_totals", model: modelId,
      admitted_at_ms: oldAdmission, input_tokens: 11, output_tokens: 7, total_tokens: 20,
      report_id: "old-0", sdk_usage: { ...native, model: "old-model-0" } }, 410);
  } finally {
    await stopRuntime(runtime);
    await rm(persistPath, { recursive: true, force: true });
  }
});
