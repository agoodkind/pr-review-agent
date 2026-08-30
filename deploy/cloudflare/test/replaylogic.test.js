import assert from "node:assert/strict";
import test from "node:test";

import {
  abandonAfterMs,
  dueEntries,
  entryFromDelivery,
  firstDelayMs,
  forwardFailed,
  maxDelayMs,
  nextWakeAt,
  replayDelayMs,
  shouldAbandon,
} from "../worker/replaylogic.js";

test("a thrown forward and a 500 both count as the container never seeing it", function () {
  assert.equal(forwardFailed(null), true);
  assert.equal(forwardFailed(new Response("", { status: 500 })), true);
});

test("every status the Go service can return counts as delivered", function () {
  for (const status of [202, 200, 400, 401, 404, 413, 502, 503]) {
    assert.equal(forwardFailed(new Response("", { status })), false, `status ${status}`);
  }
});

test("the backoff doubles from its floor and stops at its cap", function () {
  assert.equal(replayDelayMs(0), firstDelayMs);
  assert.equal(replayDelayMs(1), firstDelayMs * 2);
  assert.equal(replayDelayMs(20), maxDelayMs);
});

test("an entry is abandoned only after the window", function () {
  const entry = entryFromDelivery("d1", "/p", {}, "{}", 1_000);
  assert.equal(shouldAbandon(entry, 1_000 + abandonAfterMs - 1), false);
  assert.equal(shouldAbandon(entry, 1_000 + abandonAfterMs), true);
});

test("only entries whose backoff elapsed are due, and the alarm targets the earliest", function () {
  const early = entryFromDelivery("d1", "/p", {}, "{}", 0);
  const late = entryFromDelivery("d2", "/p", {}, "{}", 60_000);

  const due = dueEntries([early, late], firstDelayMs);
  assert.equal(due.length, 1);
  assert.equal(due[0].id, "d1");
  assert.equal(nextWakeAt([early, late]), early.notBefore);
  assert.equal(nextWakeAt([]), null);
});

test("a delivery with no identifier still gets a stable queue id", function () {
  const entry = entryFromDelivery("", "/p", {}, "{}", 0);
  assert.notEqual(entry.id, "");
});
