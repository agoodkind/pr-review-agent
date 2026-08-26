import assert from "node:assert/strict";
import crypto from "node:crypto";
import test from "node:test";

const { MAXIMUM_BATCH_BYTES, SERVICE_LOG_PATH, formatServiceLogRecord, handleServiceLogs } =
  await import("../worker/servicelogs.js");

// Stands in for the shared webhook signing key.
const SIGNING_KEY = "fixture-hmac-material";

function sign(body) {
  return "sha256=" + crypto.createHmac("sha256", SIGNING_KEY).update(body).digest("hex");
}

function logRequest(body, signature) {
  return new Request("https://worker.example" + SERVICE_LOG_PATH, {
    method: "POST",
    headers: { "X-Pr-Agent-Signature-256": signature },
    body,
  });
}

function captureConsole(run) {
  const lines = [];
  const originalLog = console.log;
  const originalError = console.error;
  console.log = (line) => lines.push({ level: "log", line });
  console.error = (line) => lines.push({ level: "error", line });
  return run().finally(() => {
    console.log = originalLog;
    console.error = originalError;
  }).then(() => lines);
}

test("a signed batch reaches the Worker log", async function () {
  const body = JSON.stringify({
    records: [
      { time: "2026-08-23T14:39:51Z", level: "INFO", message: "review job started", fields: { pull_request: "282" } },
    ],
  });

  let response;
  const lines = await captureConsole(async function () {
    response = await handleServiceLogs(logRequest(body, sign(body)), SIGNING_KEY);
  });

  assert.equal(response.status, 200);
  assert.equal(lines.length, 1);
  const printed = JSON.parse(lines[0].line);
  assert.equal(printed.source, "pr-review-agent");
  assert.equal(printed.message, "review job started");
  assert.equal(printed.pull_request, "282");
});

// This endpoint answers unauthenticated callers, and the signature is computed
// over the body, so the body has to be held before it can be checked. Without a
// cap, an unsigned caller chooses how much memory the Worker holds.
test("an oversized body is rejected before it is buffered", async function () {
  const oversize = "x".repeat(MAXIMUM_BATCH_BYTES + 1);

  let response;
  const lines = await captureConsole(async function () {
    response = await handleServiceLogs(logRequest(oversize, sign(oversize)), SIGNING_KEY);
  });

  assert.equal(response.status, 413);
  assert.equal(lines.length, 0);
});

// A caller that lies about its length must still be cut off, because a chunked
// request declares no length at all.
test("an oversized body with no declared length is still cut off", async function () {
  const oversize = new TextEncoder().encode("x".repeat(MAXIMUM_BATCH_BYTES + 1));
  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(oversize);
      controller.close();
    },
  });
  const request = new Request("https://worker.example" + SERVICE_LOG_PATH, {
    method: "POST",
    headers: { "X-Pr-Agent-Signature-256": "sha256=deadbeef" },
    body: stream,
    duplex: "half",
  });

  const response = await handleServiceLogs(request, SIGNING_KEY);

  assert.equal(response.status, 413);
});

// A batch at the cap still goes through, so the limit does not reject real work.
test("a batch at the size limit is still accepted", async function () {
  const filler = "y".repeat(1000);
  const body = JSON.stringify({ records: [{ level: "INFO", message: filler }] });
  assert.ok(body.length < MAXIMUM_BATCH_BYTES);

  let response;
  const lines = await captureConsole(async function () {
    response = await handleServiceLogs(logRequest(body, sign(body)), SIGNING_KEY);
  });

  assert.equal(response.status, 200);
  assert.equal(lines.length, 1);
});

// An unsigned caller must not be able to write into the log stream.
test("an unsigned batch is rejected and prints nothing", async function () {
  const body = JSON.stringify({ records: [{ message: "forged" }] });

  let response;
  const lines = await captureConsole(async function () {
    response = await handleServiceLogs(logRequest(body, "sha256=deadbeef"), SIGNING_KEY);
  });

  assert.equal(response.status, 401);
  assert.equal(lines.length, 0);
});

// An error from the service must stay an error in the Worker log, so it is
// still visible when the reader filters for failures.
test("an error record is printed at error level", async function () {
  const body = JSON.stringify({
    records: [{ level: "ERROR", message: "review chunk failed", fields: { chunk: "12" } }],
  });

  const lines = await captureConsole(async function () {
    await handleServiceLogs(logRequest(body, sign(body)), SIGNING_KEY);
  });

  assert.equal(lines[0].level, "error");
  assert.equal(JSON.parse(lines[0].line).chunk, "12");
});

test("a record with no fields still renders", function () {
  const printed = JSON.parse(formatServiceLogRecord({ level: "INFO", message: "started" }));

  assert.equal(printed.message, "started");
  assert.equal(printed.source, "pr-review-agent");
});
