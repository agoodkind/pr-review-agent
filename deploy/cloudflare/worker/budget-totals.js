import usageHistory from "./budget-history.sql";
import usageSnapshot from "./budget-snapshot.sql";
import usageTotals from "./budget-totals.sql";
import { MAXIMUM_USAGE_PAGE_BYTES } from "./budget.js";
import { backfillUsageTotals } from "./budget-totals-cache.js";

const MAXIMUM_PAGE_SIZE = 500;

function isNonnegativeInteger(value) {
  return Number.isSafeInteger(value) && value >= 0;
}

function validTotalsGroup(group) {
  const counters = [group.requests, group.first_admitted_at_ms,
    group.input_tokens, group.output_tokens, group.total_tokens];
  if (typeof group.reported_model !== "string" || group.reported_model === "" ||
      typeof group.service_tier !== "string" || group.requests === 0 ||
      counters.some(function (value) { return !isNonnegativeInteger(value); })) {
    return false;
  }
  for (const counter of [group.cached_input_tokens, group.cache_write_tokens]) {
    if (counter !== null && (!isNonnegativeInteger(counter) || counter > group.input_tokens)) {
      return false;
    }
  }
  return group.cached_input_tokens === null || group.cache_write_tokens === null ||
    group.cached_input_tokens <= group.input_tokens - group.cache_write_tokens;
}

export async function queryUsageTotals(storage, payload, width) {
  const { provider_id: providerId, model, start_ms: start, end_ms: end,
    include_start: includeStart, include_end: includeEnd, after_group: after,
    snapshot_sequence: requestedSnapshot, page_size: pageSize,
    retention_generation: requestedGeneration } = payload;
  if (!isNonnegativeInteger(start) || !isNonnegativeInteger(end) || start > end ||
      typeof includeStart !== "boolean" || typeof includeEnd !== "boolean" ||
      !isNonnegativeInteger(after) || !isNonnegativeInteger(requestedSnapshot) ||
      !Number.isSafeInteger(pageSize) || pageSize <= 0 || pageSize > MAXIMUM_PAGE_SIZE ||
      (requestedSnapshot === 0 && after !== 0) ||
      (requestedSnapshot !== 0 && after > requestedSnapshot) ||
      (requestedGeneration !== undefined && !isNonnegativeInteger(requestedGeneration)) ||
      (after > 0 && requestedGeneration === undefined)) {
    return new Response("invalid usage totals query", { status: 400 });
  }
  return storage.transaction(async function (transaction) {
    const cutoff = (await transaction.get("usage_retention_cutoff_ms")) ?? 0;
    const generation = (await transaction.get("usage_totals_retention_generation")) ?? 0;
    if (requestedGeneration !== undefined && requestedGeneration !== generation) {
      return new Response("usage history changed during totals pagination", { status: 409 });
    }
    const latest = storage.sql.exec(usageSnapshot).one().sequence;
    if (!isNonnegativeInteger(latest) || requestedSnapshot > latest) {
      return new Response("invalid usage snapshot", { status: 400 });
    }
    const snapshot = requestedSnapshot === 0 ? latest : requestedSnapshot;
    const initialHistoryStart = storage.sql.exec(usageHistory, providerId, Date.now()).one().history_start_ms;
    const historyStart = Math.max(initialHistoryStart, cutoff);
    backfillUsageTotals(storage, payload, width);
    const rows = storage.sql.exec(usageTotals, providerId, model, width, snapshot,
      start, Number(includeStart), end, Number(includeEnd), pageSize + 1, after).toArray();
    return renderTotalsPage(rows, pageSize, after, snapshot, historyStart, generation);
  });
}

function renderTotalsPage(rows, pageSize, after, snapshot, historyStart, generation) {
  const page = { groups: [], snapshot_sequence: snapshot, next_group: 0,
    history_start_ms: historyStart, retention_generation: generation };
  const encoder = new TextEncoder();
  let groupBytes = 0;
  for (const group of rows.slice(0, pageSize)) {
    if (!validTotalsGroup(group)) {
      return new Response("usage totals exceed the safe integer range", { status: 400 });
    }
    const next = rows.length > page.groups.length + 1 ? after + page.groups.length + 1 : 0;
    const size = encoder.encode(JSON.stringify(group)).byteLength;
    const metadataSize = encoder.encode(JSON.stringify({ ...page, groups: [], next_group: next })).byteLength;
    if (metadataSize + groupBytes + size + page.groups.length > MAXIMUM_USAGE_PAGE_BYTES) {
      if (page.groups.length === 0) {
        return new Response("usage totals group exceeds PROVIDER_USAGE_PAGE_BYTES", { status: 413 });
      }
      break;
    }
    groupBytes += size;
    page.groups.push(group);
  }
  page.next_group = rows.length > page.groups.length ? after + page.groups.length : 0;
  return Response.json(page);
}
