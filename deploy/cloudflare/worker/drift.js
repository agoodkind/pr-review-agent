import runtime from "../../../runtime.json" with { type: "json" };
import { reassessmentCallback } from "./reassessment.js";

/** @typedef {{Owner: string, Name: string}} Repository */
/** @typedef {Record<string, unknown> & {Repository: Repository, InstallationID: number, Number: number, Head: string}} ReviewJob */
/** @typedef {{key: string, version: number, job: ReviewJob, not_before_ms: number, private_outcome: Record<string, unknown>, private_nonce: string, private_delivery_id: string, private_generation: string, private_observed_at_ms: number, observation: Record<string, unknown>, observed_at_ms: number, failures: number, confirmations: number}} Target */
/** @typedef {{version: number, stage: number, installation_page: number, installation_index: number, installation_ids: number[] | null, installations_next: boolean, repository_page: number, repository_index: number, repositories: Repository[] | null, repositories_next: boolean, pull_request_page: number, pull_request_index: number, pull_requests: Record<string, unknown>[] | null, pull_requests_next: boolean, not_before_ms: number}} Sweep */
/** @typedef {Record<string, unknown> & {key: string, version: number, generation: string}} QueueRecord */
/** @typedef {{key: string, version: number}} DueEntry */
/** @typedef {{applied: boolean, target: Target | null, record: QueueRecord | null, sweep: Sweep | null}} MutationResult */
/** @typedef {{key: string, expected_target_version: number, target: Target, expected_queue_version: number, queue_version_known: boolean, record: QueueRecord | null, repair_kind: string}} Reconciliation */
/** @typedef {{sweep: Sweep, targets: Target[] | null}} InventoryPage */
/** @typedef {{version: number, generation: string}} QueueExpectation */
/** @typedef {{counts: Record<string, number>, since_ms: number, last_update_ms: number}} TransitionCounts */
/** @typedef {{getByName: (name: string) => {fetch: (request: Request) => Promise<Response>}}} FetchNamespace */
/** @typedef {{GITHUB_WEBHOOK_SECRET: string, OPERATOR_TOKEN_SHA256: string, REPLAY_QUEUE: FetchNamespace, PR_AGENT: FetchNamespace}} WorkerEnvironment */

const TARGET_PREFIX = "target:";
const DUE_PREFIX = "target-due:";
const QUEUE_PREFIX = "reassessment:";
const SWEEP_KEY = "inventory-sweep";
const COUNTS_KEY = "drift-transition-counts";
const TRANSITION_KINDS = ["reactivated", "missing_queue", "mismatched_head", "completed", "closed", "draft", "merged"];

/** @param {unknown} value @returns {value is Record<string, unknown>} */
function objectValue(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

/** @param {unknown} value @returns {value is number} */
function revisionValue(value) {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

/** @param {unknown} value @returns {value is Target} */
function targetValue(value) {
  if (!objectValue(value) || typeof value.key !== "string" || value.key === "" || !revisionValue(value.version) || !revisionValue(value.not_before_ms)) {
    return false;
  }
  if (!objectValue(value.job) || !objectValue(value.job.Repository)) {
    return false;
  }
  if (!objectValue(value.private_outcome) || !objectValue(value.observation) || typeof value.private_nonce !== "string" || typeof value.private_delivery_id !== "string" || typeof value.private_generation !== "string") {
    return false;
  }
  if (!revisionValue(value.private_observed_at_ms) || !revisionValue(value.observed_at_ms) || !revisionValue(value.failures) || !revisionValue(value.confirmations)) {
    return false;
  }
  return typeof value.job.Repository.Owner === "string" && value.job.Repository.Owner !== ""
    && typeof value.job.Repository.Name === "string" && value.job.Repository.Name !== ""
    && revisionValue(value.job.InstallationID) && value.job.InstallationID > 0
    && revisionValue(value.job.Number) && value.job.Number > 0;
}

/** @param {unknown} value @returns {value is Sweep} */
function sweepValue(value) {
  return objectValue(value) && revisionValue(value.version) && revisionValue(value.stage) && revisionValue(value.not_before_ms);
}

/** @param {string} duration @returns {number} */
export function durationMilliseconds(duration) {
  const units = { ns: 0.000001, us: 0.001, "µs": 0.001, "μs": 0.001, ms: 1, s: 1000, m: 60000, h: 3600000 };
  const normalized = duration.startsWith("+") ? duration.slice(1) : duration;
  const pattern = /([0-9]+(?:\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h)/g;
  let position = 0;
  let milliseconds = 0;
  for (const match of normalized.matchAll(pattern)) {
    if (match.index !== position) {
      throw new Error("invalid reconciliation interval");
    }
    milliseconds += Number(match[1]) * units[match[2]];
    position += match[0].length;
  }
  if (position !== normalized.length || milliseconds <= 0 || !Number.isFinite(milliseconds) || milliseconds > Number.MAX_SAFE_INTEGER) {
    throw new Error("invalid reconciliation interval");
  }
  return Math.ceil(milliseconds);
}

/** @returns {number} */
export function reconciliationIntervalMs() {
  return durationMilliseconds(runtime.REASSESSMENT.reconcile_interval);
}

/** @param {number} now @returns {Sweep} */
function initialSweep(now) {
  return {
    version: 0, stage: 0, installation_page: 1, installation_index: 0,
    installation_ids: [], installations_next: false, repository_page: 1,
    repository_index: 0, repositories: [], repositories_next: false,
    pull_request_page: 1, pull_request_index: 0, pull_requests: [],
    pull_requests_next: false, not_before_ms: now,
  };
}

/** @param {Target} target @returns {string} */
function dueKey(target) {
  return DUE_PREFIX + String(target.not_before_ms).padStart(16, "0") + ":" + encodeURIComponent(target.key);
}

/** @param {DurableObjectStorage | DurableObjectTransaction} storage @param {Target} target @param {Target | undefined} prior @returns {Promise<void>} */
async function saveTarget(storage, target, prior) {
  if (prior !== undefined) {
    await storage.delete(dueKey(prior));
  }
  await storage.put(TARGET_PREFIX + target.key, target);
  await storage.put(dueKey(target), { key: target.key, version: target.version });
}

/** @param {DurableObjectStorage} storage @param {Target} incoming @returns {Promise<MutationResult>} */
async function registerTarget(storage, incoming) {
  return storage.transaction(async function register(transaction) {
    return registerInTransaction(transaction, incoming);
  });
}

/** @param {DurableObjectTransaction} transaction @param {Target} incoming @returns {Promise<MutationResult>} */
async function registerInTransaction(transaction, incoming) {
  /** @type {Target | undefined} */
  const current = await transaction.get(TARGET_PREFIX + incoming.key);
  const target = current === undefined ? { ...incoming } : { ...current, job: incoming.job };
  target.version = (current?.version ?? 0) + 1;
  await saveTarget(transaction, target, current);
  return { applied: true, target, record: null, sweep: null };
}

/** @param {DurableObjectStorage} storage @param {Target} incoming @param {number} expected @param {QueueExpectation | null} queueExpectation @returns {Promise<MutationResult>} */
async function writeTarget(storage, incoming, expected, queueExpectation = null) {
  return storage.transaction(async function write(transaction) {
    /** @type {Target | undefined} */
    const current = await transaction.get(TARGET_PREFIX + incoming.key);
    if ((current?.version ?? 0) !== expected) {
      return { applied: false, target: current ?? null, record: null, sweep: null };
    }
    if (queueExpectation !== null) {
      /** @type {QueueRecord | undefined} */
      const queued = await transaction.get(QUEUE_PREFIX + incoming.key);
      if ((queued?.version ?? 0) !== queueExpectation.version || (queued?.generation ?? "") !== queueExpectation.generation) {
        return { applied: false, target: current ?? null, record: queued ?? null, sweep: null };
      }
    }
    const target = { ...incoming, version: expected + 1 };
    await saveTarget(transaction, target, current);
    return { applied: true, target, record: null, sweep: null };
  });
}

/** @param {DurableObjectStorage} storage @param {Sweep} incoming @param {number} expected @returns {Promise<MutationResult>} */
async function writeSweep(storage, incoming, expected) {
  return storage.transaction(async function write(transaction) {
    /** @type {Sweep | undefined} */
    const current = await transaction.get(SWEEP_KEY);
    if ((current?.version ?? 0) !== expected) {
      return { applied: false, target: null, record: null, sweep: current ?? null };
    }
    const sweep = { ...incoming, version: expected + 1 };
    await transaction.put(SWEEP_KEY, sweep);
    return { applied: true, target: null, record: null, sweep };
  });
}

/** @param {DurableObjectStorage} storage @param {number} now @returns {Promise<MutationResult>} */
export async function ensureSweep(storage, now) {
  return storage.transaction(async function ensure(transaction) {
    await ensureTransitionCounts(transaction, now);
    /** @type {Sweep | undefined} */
    let sweep = await transaction.get(SWEEP_KEY);
    if (sweep === undefined) {
      sweep = { ...initialSweep(now), version: 1 };
      await transaction.put(SWEEP_KEY, sweep);
      return { applied: true, target: null, record: null, sweep };
    }
    return { applied: false, target: null, record: null, sweep };
  });
}

/** @param {DurableObjectTransaction} transaction @param {number} now @returns {Promise<TransitionCounts>} */
async function ensureTransitionCounts(transaction, now) {
  /** @type {TransitionCounts | undefined} */
  let counts = await transaction.get(COUNTS_KEY);
  if (counts === undefined) {
    counts = { counts: Object.fromEntries(TRANSITION_KINDS.map(function zero(kind) { return [kind, 0]; })), since_ms: now, last_update_ms: now };
    await transaction.put(COUNTS_KEY, counts);
  }
  return counts;
}

/** @param {unknown} left @param {unknown} right @returns {boolean} */
function sameJSON(left, right) {
  if (left === right) {
    return true;
  }
  if (Array.isArray(left) && Array.isArray(right)) {
    return left.length === right.length && left.every(function same(item, index) { return sameJSON(item, right[index]); });
  }
  if (!objectValue(left) || !objectValue(right)) {
    return false;
  }
  const keys = Object.keys(left);
  return keys.length === Object.keys(right).length && keys.every(function same(key) { return Object.hasOwn(right, key) && sameJSON(left[key], right[key]); });
}

/** @param {DurableObjectTransaction} transaction @param {QueueRecord | undefined} prior @param {QueueRecord} record @param {string} repairKind @returns {Promise<void>} */
export async function countQueueTransition(transaction, prior, record, repairKind = "") {
  const before = prior === undefined ? null : { ...prior, version: 0 };
  if (before !== null && sameJSON(before, { ...record, version: 0 })) {
    return;
  }
  let kind = repairKind;
  if (kind === "" && record.reason === "reactivated" && prior?.generation !== record.generation) {
    kind = "reactivated";
  }
  if (!TRANSITION_KINDS.includes(kind)) {
    return;
  }
  const counts = await ensureTransitionCounts(transaction, Date.now());
  counts.counts[kind] += 1;
  counts.last_update_ms = Date.now();
  await transaction.put(COUNTS_KEY, counts);
}

/** @param {DurableObjectStorage} storage @param {InventoryPage} page @param {number} expected @returns {Promise<MutationResult>} */
async function applyInventory(storage, page, expected) {
  return storage.transaction(async function apply(transaction) {
    /** @type {Sweep | undefined} */
    const current = await transaction.get(SWEEP_KEY);
    if ((current?.version ?? 0) !== expected) {
      return { applied: false, target: null, record: null, sweep: current ?? null };
    }
    for (const target of page.targets) {
      await registerInTransaction(transaction, target);
    }
    const sweep = { ...page.sweep, version: expected + 1 };
    await transaction.put(SWEEP_KEY, sweep);
    return { applied: true, target: null, record: null, sweep };
  });
}

/** @param {DurableObjectStorage} storage @param {Reconciliation} mutation @returns {Promise<MutationResult>} */
async function applyReconciliation(storage, mutation) {
  return storage.transaction(async function apply(transaction) {
    /** @type {Target | undefined} */
    const current = await transaction.get(TARGET_PREFIX + mutation.key);
    /** @type {QueueRecord | undefined} */
    const queued = await transaction.get(QUEUE_PREFIX + mutation.key);
    if ((current?.version ?? 0) !== mutation.expected_target_version || (mutation.queue_version_known && (queued?.version ?? 0) !== mutation.expected_queue_version)) {
      return { applied: false, target: current ?? null, record: queued ?? null, sweep: null };
    }
    const target = { ...mutation.target, version: mutation.expected_target_version + 1 };
    await saveTarget(transaction, target, current);
    let record = queued ?? null;
    if (mutation.record !== null) {
      record = { ...mutation.record, version: mutation.expected_queue_version + 1 };
      await transaction.put(QUEUE_PREFIX + mutation.key, record);
      await countQueueTransition(transaction, queued, record, mutation.repair_kind);
    }
    return { applied: true, target, record, sweep: null };
  });
}

/** @param {DurableObjectStorage} storage @param {Record<string, unknown>} payload @returns {Promise<Response | null>} */
export async function driftMutation(storage, payload) {
  if (payload.action === "ensure_sweep") {
    return Response.json(await ensureSweep(storage, Date.now()));
  }
  if (payload.action === "read_sweep") {
    const sweep = await storage.get(SWEEP_KEY);
    return Response.json({ applied: false, target: null, record: null, sweep: sweep ?? null });
  }
  if (payload.action === "query_target" && typeof payload.key === "string" && payload.key !== "") {
    const target = await storage.get(TARGET_PREFIX + payload.key);
    return Response.json({ applied: false, target: target ?? null, record: null, sweep: null });
  }
  if (payload.action === "register_target" || payload.action === "write_target") {
    if (!targetValue(payload.target) || payload.target.key !== payload.key) {
      return new Response("invalid target", { status: 400 });
    }
    if (payload.action === "register_target") {
      return Response.json(await registerTarget(storage, payload.target));
    }
    if (!revisionValue(payload.expected_version) || !revisionValue(payload.expected_queue_version) || typeof payload.expected_queue_generation !== "string") {
      return new Response("invalid target revision", { status: 400 });
    }
    return Response.json(await writeTarget(storage, payload.target, payload.expected_version, { version: payload.expected_queue_version, generation: payload.expected_queue_generation }));
  }
  if (payload.action === "write_sweep") {
    if (!sweepValue(payload.sweep) || !revisionValue(payload.expected_version)) {
      return new Response("invalid sweep", { status: 400 });
    }
    return Response.json(await writeSweep(storage, payload.sweep, payload.expected_version));
  }
  if (payload.action === "apply_reconciliation") {
    if (!targetValue(payload.target) || payload.target.key !== payload.key || !revisionValue(payload.expected_target_version) || !revisionValue(payload.expected_queue_version) || typeof payload.queue_version_known !== "boolean" || typeof payload.repair_kind !== "string") {
      return new Response("invalid reconciliation", { status: 400 });
    }
    if (payload.record !== null && (!objectValue(payload.record) || payload.record.key !== payload.key)) {
      return new Response("invalid reconciliation record", { status: 400 });
    }
    if (!payload.queue_version_known && payload.record !== null) {
      return new Response("queue mutation requires a known revision", { status: 400 });
    }
    return Response.json(await applyReconciliation(storage, /** @type {Reconciliation} */ (payload)));
  }
  return null;
}

/** @param {DurableObjectStorage} storage @returns {Promise<{targets: Target[], sweep: Sweep | null, next_alarm_ms: number | null, transition_counts: TransitionCounts | null}>} */
export async function driftSnapshot(storage) {
  /** @type {Map<string, Target>} */
  const targets = await storage.list({ prefix: TARGET_PREFIX });
  /** @type {Sweep | undefined} */
  const sweep = await storage.get(SWEEP_KEY);
  const transitionCounts = await storage.get(COUNTS_KEY);
  return { targets: [...targets.values()], sweep: sweep ?? null, next_alarm_ms: await storage.getAlarm(), transition_counts: transitionCounts ?? null };
}

/** @param {DurableObjectStorage} storage @returns {Promise<number | null>} */
export async function nextDriftWake(storage) {
  if (!runtime.REASSESSMENT.enabled) {
    return null;
  }
  const ensured = await ensureSweep(storage, Date.now());
  let earliest = ensured.sweep.not_before_ms;
  /** @type {Map<string, DueEntry>} */
  const due = await storage.list({ prefix: DUE_PREFIX, limit: 1 });
  for (const entry of due.values()) {
    /** @type {Target | undefined} */
    const target = await storage.get(TARGET_PREFIX + entry.key);
    if (target !== undefined) {
      earliest = Math.min(earliest, target.not_before_ms);
    }
  }
  return earliest;
}

/** @param {DurableObjectStorage} storage @param {WorkerEnvironment} env @param {number} now @returns {Promise<void>} */
export async function reconcileDrift(storage, env, now) {
  if (!runtime.REASSESSMENT.enabled) {
    return;
  }
  const ensured = await ensureSweep(storage, now);
  const sweep = ensured.sweep;
  if (sweep.not_before_ms <= now) {
    try {
      const response = await reassessmentCallback(env, { action: "inventory", sweep });
      if (!response.ok) {
        throw new Error("inventory callback failed");
      }
      /** @type {InventoryPage} */
      const page = await response.json();
      if (!sweepValue(page.sweep) || (page.targets !== null && !Array.isArray(page.targets)) || page.targets?.some(function invalid(target) { return !targetValue(target); })) {
        throw new Error("inventory callback returned invalid metadata");
      }
      await applyInventory(storage, { ...page, targets: page.targets ?? [] }, sweep.version);
    } catch {
      await writeSweep(storage, { ...sweep, not_before_ms: now + reconciliationIntervalMs() }, sweep.version);
      console.error(JSON.stringify({ message: "reassessment inventory callback failed", err: "inventory response unavailable or invalid" }));
    }
  }
  /** @type {Map<string, DueEntry>} */
  const entries = await storage.list({ prefix: DUE_PREFIX, end: DUE_PREFIX + String(now + 1).padStart(16, "0"), limit: runtime.REASSESSMENT.batch_size });
  for (const entry of entries.values()) {
    /** @type {Target | undefined} */
    const target = await storage.get(TARGET_PREFIX + entry.key);
    if (target === undefined || target.version !== entry.version || target.not_before_ms > now) {
      continue;
    }
    try {
      const response = await reassessmentCallback(env, { action: "reconcile_target", target });
      if (!response.ok) {
        throw new Error("reconciliation callback failed");
      }
      /** @type {Reconciliation} */
      const mutation = await response.json();
      if (mutation.key !== target.key || mutation.expected_target_version !== target.version) {
        throw new Error("reconciliation callback returned a different target revision");
      }
      const applied = await driftMutation(storage, { ...mutation, action: "apply_reconciliation" });
      if (applied === null || !applied.ok) {
        throw new Error("reconciliation callback returned invalid metadata");
      }
      /** @type {MutationResult} */
      const result = await applied.json();
      if (result.applied && mutation.repair_kind !== "") {
        console.log(JSON.stringify({ message: "reassessment drift repaired", target_key: target.key, target_version: result.target.version, repair_kind: mutation.repair_kind }));
      }
    } catch {
      await writeTarget(storage, { ...target, not_before_ms: now + reconciliationIntervalMs() }, target.version);
      console.error(JSON.stringify({ message: "reassessment target callback failed", target_key: target.key, err: "target response unavailable or invalid" }));
    }
  }
}
