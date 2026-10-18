function activeReviewService(env) {
  if (env.REVIEW_SERVICE_MODE === "legacy") {
    if (!env.PR_AGENT) {
      throw new Error("legacy review service requires PR_AGENT");
    }
    return env.PR_AGENT.getByName("github-app");
  }
  if (env.REVIEW_SERVICE_MODE === "service") {
    if (!env.REVIEW_SERVICE) {
      throw new Error("service review mode requires REVIEW_SERVICE");
    }
    return env.REVIEW_SERVICE;
  }
  throw new Error("REVIEW_SERVICE_MODE must be legacy or service");
}

export function fetchActiveReviewService(request, env) {
  return activeReviewService(env).fetch(request);
}

export function interruptActiveReviewService(env) {
  return activeReviewService(env).interruptForRecoveryTest();
}
