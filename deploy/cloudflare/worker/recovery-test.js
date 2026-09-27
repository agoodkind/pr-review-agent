import recoveryTest from "../recovery-test.json" with { type: "json" };

export { recoveryTest };

export function isRecoveryTestEvent(eventType, body) {
  if (eventType !== "pull_request") {
    return false;
  }

  let payload;
  try {
    payload = JSON.parse(body);
  } catch {
    return false;
  }

  return payload.action === "labeled" &&
    payload.repository?.full_name === recoveryTest.repository &&
    payload.pull_request?.number === recoveryTest.pullRequest &&
    payload.label?.name === recoveryTest.interruptLabel &&
    Array.isArray(payload.pull_request?.labels) &&
    payload.pull_request.labels.some(function isForceLabel(label) {
      return label.name === recoveryTest.forceLabel;
    });
}
