import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createHmac } from "node:crypto";
import { mkdtemp, rm } from "node:fs/promises";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const cloudflareDirectory = fileURLToPath(new URL("..", import.meta.url));
const hour = 3_600_000;
const model = "measured_model";

async function unusedPort() {
  const server = net.createServer();
  await new Promise(function (resolve) { server.listen(0, "127.0.0.1", resolve); });
  const port = server.address().port;
  await new Promise(function (resolve) { server.close(resolve); });
  return port;
}

async function stopRuntime(child) {
  if (child.exitCode !== null || child.signalCode !== null) {
    return;
  }
  await new Promise(function (resolve) {
    child.once("exit", resolve);
    child.kill("SIGINT");
  });
}

async function startRuntime(context, options = {}) {
  const port = options.port ?? await unusedPort();
  const persistPath = options.persistPath ?? await mkdtemp(path.join(os.tmpdir(), "pr-agent-budget-rows-"));
  const child = spawn(path.join(cloudflareDirectory, "node_modules/.bin/wrangler"), [
    "dev", "--config", options.configuration ?? "test/budget-rows.wrangler.jsonc", "--local",
    "--port", String(port), "--inspector-port", "0", "--persist-to", persistPath,
    "--var", "PROVIDER_USAGE_TOTAL_BUCKET_WIDTH:1h",
    "--var", 'PROVIDER_USAGE_RETENTION:{"max_age":"0s","cleanup_interval":"1h","batch_size":1}',
  ], { cwd: cloudflareDirectory, stdio: ["ignore", "pipe", "pipe"] });
  context.after(async function () {
    await stopRuntime(child);
    if (options.persistPath === undefined) {
      await rm(persistPath, { recursive: true, force: true });
    }
  });
  await new Promise(function (resolve, reject) {
    let output = "";
    const timer = setTimeout(function () {
      reject(new Error("budget measurement worker did not start: " + output));
    }, 20000);
    function onOutput(data) {
      output += data.toString();
      if (output.includes("Ready on")) {
        clearTimeout(timer);
        resolve();
      }
    }
    child.stdout.on("data", onOutput);
    child.stderr.on("data", onOutput);
    child.once("error", function (error) { clearTimeout(timer); reject(error); });
    child.once("exit", function (code) {
      clearTimeout(timer);
      reject(new Error("budget measurement worker exited " + code + ": " + output));
    });
  });
  return { port, child };
}

async function request(port, payload, pathname = "/internal/v1/provider_budget") {
  const body = JSON.stringify(payload);
  const signature = "sha256=" + createHmac("sha256", "test-review-budget-secret")
    .update(body).digest("hex");
  const response = await fetch(`http://127.0.0.1:${port}${pathname}`, {
    method: "POST", headers: { "X-Pr-Agent-Budget-Signature": signature }, body,
  });
  assert.equal(response.status, 200, response.ok ? undefined : await response.text());
  if (pathname !== "/__test/seed") {
    assert.ok(response.headers.has("X-Test-SQL-Rows-Read"));
    assert.ok(response.headers.has("X-Test-SQL-Rows-Written"));
  }
  return {
    page: await response.json(),
    rowsRead: Number(response.headers.get("X-Test-SQL-Rows-Read")),
    rowsWritten: Number(response.headers.get("X-Test-SQL-Rows-Written")),
  };
}

function totals(port, provider, start, end, options = {}) {
  return request(port, {
    action: "query_usage_totals", provider_id: provider, model,
    start_ms: start, end_ms: end, include_start: true, include_end: true,
    after_group: 0, snapshot_sequence: 0, page_size: 500, ...options,
  });
}

function report(port, provider, admittedAt, options = {}) {
  return request(port, {
    action: "report_usage", provider_id: provider, model, admitted_at_ms: admittedAt,
    input_tokens: 11, output_tokens: 7, total_tokens: 20, ...options,
  });
}

function assertCounters(page, count, firstAdmission) {
  assert.equal(page.groups.length, 1);
  const group = page.groups[0];
  assert.deepEqual({
    requests: group.requests, first: group.first_admitted_at_ms,
    input: group.input_tokens, output: group.output_tokens, total: group.total_tokens,
    cached: group.cached_input_tokens, writes: group.cache_write_tokens,
  }, {
    requests: count, first: firstAdmission,
    input: count * 11, output: count * 7, total: count * 20,
    cached: null, writes: null,
  });
}

test("cached totals and reports bound real SQL reads independently of event history", async function (context) {
  const { port } = await startRuntime(context);
  const start = Math.floor((Date.now() - 2 * hour) / hour) * hour;
  const measurements = [];
  for (const [provider, count] of [["small_history", 100], ["large_history", 10000]]) {
    await request(port, { count, provider, model, start: start + 1000 }, "/__test/seed");
    const firstAdmission = start + 1001;
    const end = start + hour;
    const cold = await totals(port, provider, start, end);
    assertCounters(cold.page, count, firstAdmission);
    const warm = await totals(port, provider, start, end);
    const repeated = await totals(port, provider, start, end);
    assert.deepEqual(warm.page, cold.page);
    assert.deepEqual(repeated.page, warm.page);
    const actualBounds = await totals(port, provider, firstAdmission, start + 1000 + count);
    assert.deepEqual(actualBounds.page.groups, cold.page.groups);
    for (const [includeStart, includeEnd] of [[false, true], [true, false], [false, false]]) {
      const bounded = await totals(port, provider, firstAdmission, start + 1000 + count, {
        include_start: includeStart, include_end: includeEnd,
      });
      assertCounters(bounded.page, count - Number(!includeStart) - Number(!includeEnd),
        firstAdmission + Number(!includeStart));
    }

    const insertion = await report(port, provider, start + 500, { report_id: provider + "_late" });
    assert.deepEqual(insertion.page, { reported: true, sdk_usage_stored: false });
    const updated = await totals(port, provider, start, end);
    assertCounters(updated.page, count + 1, start + 500);
    const historical = await totals(port, provider, start, end, {
      snapshot_sequence: cold.page.snapshot_sequence,
      retention_generation: cold.page.retention_generation,
    });
    assert.deepEqual(historical.page.groups, cold.page.groups);
    measurements.push({ events: count, cold: cold.rowsRead, warm: warm.rowsRead,
      repeated: repeated.rowsRead, actualBounds: actualBounds.rowsRead,
      report: insertion.rowsRead, updated: updated.rowsRead,
      coldWrites: cold.rowsWritten, warmWrites: warm.rowsWritten,
      repeatedWrites: repeated.rowsWritten, reportWrites: insertion.rowsWritten,
      updatedWrites: updated.rowsWritten });
  }

  const beforeUnrelated = await totals(port, "large_history", start, start + hour);
  await request(port, { count: 10000, provider: "unrelated_history", model,
    start: start + 1000 }, "/__test/seed");
  const afterUnrelated = await totals(port, "large_history", start, start + hour);
  assert.deepEqual(afterUnrelated.page.groups, beforeUnrelated.page.groups);

  const native = { api: "responses", model: "z_reported_model",
    usage_json: '{"future_integer":123456789012345678901234567890,"future":[null,true]}' };
  const day = new Date().toISOString().slice(0, 10);
  await report(port, "large_history", start + 400, { report_id: "new_group",
    sdk_usage: native, legacy_day: day, legacy_tokens: 20 });
  const first = await totals(port, "large_history", start, start + hour, { page_size: 1 });
  assert.equal(first.page.next_group, 1);
  assert.equal(first.page.groups[0].requests, 10001);
  const secondOptions = { page_size: 1, after_group: first.page.next_group,
    snapshot_sequence: first.page.snapshot_sequence,
    retention_generation: first.page.retention_generation };
  const second = await totals(port, "large_history", start, start + hour, secondOptions);
  const secondRepeated = await totals(port, "large_history", start, start + hour, secondOptions);
  assert.deepEqual(secondRepeated.page, second.page);
  assert.equal(second.page.next_group, 0);
  assert.deepEqual([second.page.groups[0].reported_model, second.page.groups[0].requests,
    second.page.groups[0].total_tokens], [native.model, 1, 20]);
  const raw = await request(port, { action: "query_usage", provider_id: "large_history", model,
    start_ms: start + 400, end_ms: start + 400, include_start: true, include_end: true,
    after_sequence: 0, snapshot_sequence: 0, page_size: 1 });
  assert.equal(raw.page.events.length, 1);
  assert.deepEqual(raw.page.events[0].sdk_usage, native);
  const daily = await request(port, { provider_id: "large_history", model, limit: 20 });
  assert.deepEqual([daily.page.used, daily.page.remaining, daily.page.allowed], [20, 0, false]);

  context.diagnostic(JSON.stringify({ measurements,
    unrelatedProvider: { before: beforeUnrelated.rowsRead, after: afterUnrelated.rowsRead,
      beforeWrites: beforeUnrelated.rowsWritten, afterWrites: afterUnrelated.rowsWritten },
    totalsPages: { first: first.rowsRead, second: second.rowsRead, repeated: secondRepeated.rowsRead,
      firstWrites: first.rowsWritten, secondWrites: second.rowsWritten,
      repeatedWrites: secondRepeated.rowsWritten } }));
  for (const measurement of measurements) {
    for (const operation of ["warm", "repeated", "actualBounds", "report", "updated"]) {
      assert.ok(measurement[operation] <= 200,
        `${operation} read ${measurement[operation]} SQL rows for ${measurement.events} events`);
    }
    assert.ok(measurement.cold > measurement.warm,
      "Existing uncached history requires one initial backfill.");
    for (const operation of ["warmWrites", "repeatedWrites", "updatedWrites"]) {
      assert.equal(measurement[operation], 0,
        `${operation} wrote SQL rows for ${measurement.events} events`);
    }
    assert.ok(measurement.reportWrites <= 30);
  }
  for (const operation of ["warm", "repeated", "actualBounds", "report", "updated",
    "warmWrites", "repeatedWrites", "reportWrites", "updatedWrites"]) {
    assert.ok(measurements[1][operation] <= measurements[0][operation] + 20,
      `${operation} SQL reads grew with event history`);
  }
  assert.ok(afterUnrelated.rowsRead <= beforeUnrelated.rowsRead + 20,
    "Another provider's history increased SQL reads for cached totals.");
  assert.equal(afterUnrelated.rowsWritten, beforeUnrelated.rowsWritten);
  for (const result of [first, second, secondRepeated]) {
    assert.ok(result.rowsRead <= 200, `A cached totals page read ${result.rowsRead} SQL rows`);
    assert.equal(result.rowsWritten, 0);
  }
});

test("persisted totals rebuild after a legacy writer reports during rollback", async function (context) {
  const persistPath = await mkdtemp(path.join(os.tmpdir(), "pr-agent-budget-rollback-"));
  const port = await unusedPort();
  const start = Math.floor((Date.now() - 2 * hour) / hour) * hour;
  const native = { api: "responses", usage_json: '{"future_count":123456789012345678901234567890,"future":[null,true]}' };
  let runtime;
  try {
    runtime = await startRuntime(context, { port, persistPath });
    await report(port, "rollback", start + 1000, { report_id: "before_rollback", sdk_usage: native });
    const before = await totals(port, "rollback", start, start + hour);
    assertCounters(before.page, 1, start + 1000);
    await stopRuntime(runtime.child);

    runtime = await startRuntime(context, { port, persistPath,
      configuration: "test/budget-legacy.wrangler.jsonc" });
    const legacy = await fetch(`http://127.0.0.1:${port}/report_usage`, {
      method: "POST", body: JSON.stringify({ provider_id: "rollback", model,
        admitted_at_ms: start + 500, input_tokens: 11, output_tokens: 7, total_tokens: 20,
        sdk_usage: native, bucket_width_ms: hour }),
    });
    assert.equal(legacy.status, 200, legacy.ok ? undefined : await legacy.text());
    assert.deepEqual(await legacy.json(), { reported: true, sdk_usage_stored: true });
    await stopRuntime(runtime.child);

    runtime = await startRuntime(context, { port, persistPath });
    const raw = await request(port, { action: "query_usage", provider_id: "rollback", model,
      start_ms: start, end_ms: start + hour, include_start: true, include_end: true,
      after_sequence: 0, snapshot_sequence: 0, page_size: 500 });
    assert.deepEqual(raw.page.events.map(function (event) {
      return [event.admitted_at_ms, event.input_tokens, event.output_tokens, event.total_tokens,
        event.sdk_usage];
    }), [[start + 1000, 11, 7, 20, native], [start + 500, 11, 7, 20, native]]);
    const rebuilt = await totals(port, "rollback", start, start + hour);
    assertCounters(rebuilt.page, 2, start + 500);
    const warm = await totals(port, "rollback", start, start + hour);
    assert.deepEqual(warm.page, rebuilt.page);
    assert.ok(warm.rowsRead <= 200);
    assert.equal(warm.rowsWritten, 0);
    context.diagnostic(JSON.stringify({ rollback: { rebuildReads: rebuilt.rowsRead,
      rebuildWrites: rebuilt.rowsWritten, warmReads: warm.rowsRead, warmWrites: warm.rowsWritten } }));
  } finally {
    if (runtime !== undefined) {
      await stopRuntime(runtime.child);
    }
    await rm(persistPath, { recursive: true, force: true });
  }
});
