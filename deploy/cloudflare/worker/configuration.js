// REVIEW_SETTINGS_HEADER carries the review tuning values on each forwarded
// delivery, so a corrected value governs the next review rather than waiting for
// the process to be replaced.
export const REVIEW_SETTINGS_HEADER = "X-Pr-Agent-Review-Settings";

// REVIEW_SETTINGS_SIGNATURE_HEADER authenticates those values.
//
// The webhook signature covers the request body and nothing else, so it says
// nothing about a header travelling beside it. Treating a verified body as
// authority over the headers would let anyone who can put a request in front of
// the container set a chunk timeout that fails every review, or an importance
// floor that suppresses every finding while the verdict still reports success.
// The values carry their own signature instead.
export const REVIEW_SETTINGS_SIGNATURE_HEADER = "X-Pr-Agent-Review-Settings-Signature";

// signReviewSettings binds the tuning values to the body they travel with.
//
// The body is part of what is signed, so a signature is worth nothing on any
// other delivery: lifting a valid pair off one request and replaying it in front
// of another fails, because the digest covers a body that is no longer there.
export async function signReviewSettings(signingKey, settings, body) {
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(signingKey),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const signed = new TextEncoder().encode(settings + "\n" + body);
  const digest = await crypto.subtle.sign("HMAC", key, signed);
  const hex = [...new Uint8Array(digest)]
    .map(function (byte) {
      return byte.toString(16).padStart(2, "0");
    })
    .join("");
  return "sha256=" + hex;
}

// createReviewSettingsHeader renders the tuning values the service applies per
// review, or the empty string when this worker has none configured.
//
// Only values that govern one review travel. The model and the worker count size
// the process rather than the review, and no secret travels at all: this rides
// beside the signed body rather than inside it, so nothing here may be worth
// more than the harm it could do if a stranger chose it.
//
// A value this worker does not have is left out rather than sent empty, because
// the service reads an absent field as its own configuration standing. That is
// what lets a worker and a container at different versions work together.
export function createReviewSettingsHeader(bindings) {
  const settings = {};
  const minimumImportance = readPositiveInteger(bindings.REVIEW_MIN_IMPORTANCE);
  if (minimumImportance !== null) {
    settings.minimum_importance = minimumImportance;
  }
  const maxFiles = readPositiveInteger(bindings.REVIEW_MAX_FILES);
  if (maxFiles !== null) {
    settings.max_files = maxFiles;
  }
  const maxChunks = readPositiveInteger(bindings.REVIEW_MAX_CHUNKS);
  if (maxChunks !== null) {
    settings.max_chunks = maxChunks;
  }
  if (typeof bindings.REVIEW_CHUNK_TIMEOUT === "string" && bindings.REVIEW_CHUNK_TIMEOUT !== "") {
    settings.chunk_timeout = bindings.REVIEW_CHUNK_TIMEOUT;
  }
  if (Object.keys(settings).length === 0) {
    return "";
  }
  return JSON.stringify(settings);
}

// readPositiveInteger returns a binding as a whole number above zero, or null
// when it is anything else.
//
// Bindings arrive as strings and a misconfigured one is whatever somebody typed.
// A zero or a negative would disable a budget, and the service refuses one for
// that reason; not sending it at all says the same thing earlier and leaves the
// service on the value it booted with.
function readPositiveInteger(value) {
  if (typeof value !== "string" && typeof value !== "number") {
    return null;
  }
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed <= 0) {
    return null;
  }
  return parsed;
}

// The container receives only the bindings named here. A binding that is not
// destructured and re-emitted never reaches the service, so a new variable must
// be added in both places.
export function createPrAgentEnvironment(bindings) {
  const {
    CF_ACCESS_CLIENT_ID,
    CF_ACCESS_CLIENT_SECRET,
    CLYDE_BASE_URL,
    FALLBACK_API_KEY,
    FALLBACK_BASE_URL,
    FALLBACK_CF_ACCESS_CLIENT_ID,
    FALLBACK_CF_ACCESS_CLIENT_SECRET,
    FALLBACK_MODEL,
    FALLBACK_ON,
    GITHUB_APP_ID,
    GITHUB_BOT_LOGIN,
    GITHUB_PRIVATE_KEY,
    GITHUB_WEBHOOK_SECRET,
    LOG_FORWARD_URL,
    OPENAI_KEY,
    PORT,
    REVIEW_CHUNK_TIMEOUT,
    REVIEW_MAX_CHUNKS,
    REVIEW_MAX_FILES,
    REVIEW_MIN_IMPORTANCE,
    REVIEW_MODEL,
    REVIEW_WORKERS,
  } = bindings;

  return {
    CF_ACCESS_CLIENT_ID,
    CF_ACCESS_CLIENT_SECRET,
    CLYDE_API_KEY: OPENAI_KEY,
    CLYDE_BASE_URL,
    FALLBACK_API_KEY,
    FALLBACK_BASE_URL,
    FALLBACK_CF_ACCESS_CLIENT_ID,
    FALLBACK_CF_ACCESS_CLIENT_SECRET,
    FALLBACK_MODEL,
    FALLBACK_ON,
    GITHUB_APP_ID,
    GITHUB_BOT_LOGIN,
    GITHUB_PRIVATE_KEY,
    GITHUB_WEBHOOK_SECRET,
    LOG_FORWARD_URL,
    PORT,
    REVIEW_CHUNK_TIMEOUT,
    REVIEW_MAX_CHUNKS,
    REVIEW_MAX_FILES,
    REVIEW_MIN_IMPORTANCE,
    REVIEW_MODEL,
    REVIEW_WORKERS,
  };
}
