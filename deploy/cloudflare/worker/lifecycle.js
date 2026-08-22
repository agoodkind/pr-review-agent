// The container writes its logs to stdout, which nothing outside the container
// can read. These events are the only record that a container started, stopped,
// or was stopped for idleness, and they go to the Worker log where wrangler
// tail and Workers Logs can see them.
export function containerLifecycleEvent(message, details = {}) {
  return JSON.stringify({ message, ...details });
}

// A stop reports its exit code and reason, because a container that dies mid
// review otherwise leaves an unexplained failure on the pull request.
export function containerStoppedEvent(params) {
  return containerLifecycleEvent("container stopped", {
    exitCode: params?.exitCode ?? null,
    reason: params?.reason ?? "unknown",
  });
}
