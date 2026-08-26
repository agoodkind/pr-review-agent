// The Go review service runs inside a container whose stdout reaches no log
// sink. It ships its own logs here, and this module prints them so they land in
// Workers Logs alongside the Worker's own output.

export const SERVICE_LOG_PATH = "/internal/v1/service_logs";
const SIGNATURE_HEADER = "X-Pr-Agent-Signature-256";

// MAXIMUM_BATCH_BYTES caps one shipped batch. The service batches at most 64
// records, so a real batch is far smaller; the cap exists because this endpoint
// answers unauthenticated callers.
export const MAXIMUM_BATCH_BYTES = 1024 * 1024;

// readBoundedText buffers at most limit bytes and returns null past that, so an
// unauthenticated caller cannot choose how much memory this Worker holds. The
// declared length is checked first to reject early, and the stream is measured
// as it arrives because a chunked request declares no length at all.
export async function readBoundedText(request, limit) {
  const declared = Number(request.headers.get("content-length"));
  if (Number.isFinite(declared) && declared > limit) {
    return null;
  }
  if (!request.body) {
    return "";
  }

  const reader = request.body.getReader();
  const chunks = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) {
      break;
    }
    total += value.byteLength;
    if (total > limit) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }

  const joined = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    joined.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(joined);
}

// verifyServiceLogSignature checks the batch signature with the same key and
// scheme GitHub uses for webhooks, so shipping logs needs no extra credential.
export async function verifyServiceLogSignature(signingKey, body, signature) {
  if (!signingKey || !signature || !signature.startsWith("sha256=")) {
    return false;
  }
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(signingKey),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const digest = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(body));
  const expected = "sha256=" + [...new Uint8Array(digest)].map(toHex).join("");
  return timingSafeEqual(expected, signature);
}

function toHex(byte) {
  return byte.toString(16).padStart(2, "0");
}

// timingSafeEqual compares two equal-length strings without leaking how many
// leading characters matched.
function timingSafeEqual(left, right) {
  if (left.length !== right.length) {
    return false;
  }
  let difference = 0;
  for (let index = 0; index < left.length; index++) {
    difference |= left.charCodeAt(index) ^ right.charCodeAt(index);
  }
  return difference === 0;
}

// formatServiceLogRecord renders one forwarded record for the Worker log. The
// service name is stamped on every line so a reader can tell a container log
// from the Worker's own.
export function formatServiceLogRecord(record) {
  return JSON.stringify({
    source: "pr-review-agent",
    time: record?.time ?? null,
    level: record?.level ?? "INFO",
    message: record?.message ?? "",
    ...(record?.fields ?? {}),
  });
}

// handleServiceLogs verifies and prints one shipped batch. A rejected batch
// returns 401 and prints nothing, so an unsigned caller cannot write into the
// log stream.
export async function handleServiceLogs(request, signingKey) {
  if (request.method !== "POST") {
    return new Response("method not allowed", { status: 405 });
  }
  // The size limit is enforced before the signature, because the signature is
  // computed over the body and computing it requires holding the body. Reading
  // the whole body first would let an unsigned caller decide how much memory
  // this Worker holds.
  const body = await readBoundedText(request, MAXIMUM_BATCH_BYTES);
  if (body === null) {
    return new Response("batch too large", { status: 413 });
  }
  const signature = request.headers.get(SIGNATURE_HEADER) ?? "";
  if (!(await verifyServiceLogSignature(signingKey, body, signature))) {
    return new Response("invalid signature", { status: 401 });
  }

  let batch;
  try {
    batch = JSON.parse(body);
  } catch {
    return new Response("malformed batch", { status: 400 });
  }

  const records = Array.isArray(batch?.records) ? batch.records : [];
  for (const record of records) {
    const line = formatServiceLogRecord(record);
    if (record?.level === "ERROR") {
      console.error(line);
    } else {
      console.log(line);
    }
  }
  return Response.json({ accepted: records.length });
}
