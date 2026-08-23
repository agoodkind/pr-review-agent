import assert from "node:assert/strict";
import test from "node:test";

const { containerLifecycleEvent, containerStoppedEvent } = await import("../worker/lifecycle.js");

test("a container stop reports its exit code and reason", function () {
  const event = JSON.parse(containerStoppedEvent({ exitCode: 137, reason: "runtime_signal" }));

  assert.equal(event.message, "container stopped");
  assert.equal(event.exitCode, 137);
  assert.equal(event.reason, "runtime_signal");
});

// The library calls onStop with no params on some paths, and a stop that
// reports nothing is the silence that made an aborted review unexplainable.
test("a stop with no details still reports that it happened", function () {
  const event = JSON.parse(containerStoppedEvent(undefined));

  assert.equal(event.message, "container stopped");
  assert.equal(event.reason, "unknown");
});

test("an idle expiry names the threshold that fired it", function () {
  const event = JSON.parse(containerLifecycleEvent("container idle timer expired", { sleepAfter: "11m" }));

  assert.equal(event.message, "container idle timer expired");
  assert.equal(event.sleepAfter, "11m");
});
