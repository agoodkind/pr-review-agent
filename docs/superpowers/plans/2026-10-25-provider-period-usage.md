# Report provider usage for the current period

> Use `executing-plans` to implement these tasks in dependency order.

**Goal:** Add a configurable Review details column that reports measured tokens and estimated cost across reviews for each provider and requested model.

**Architecture:** Reuse persistent quota event history and its existing window calculation. Load one snapshot per represented provider/model at final publication after usage reporting completes. Keep those totals separate from the current review's usage and display their time range and measurement coverage.

**Tech Stack:** Go, existing provider SDK adapters, Cloudflare accounting storage, and existing pricing configuration.

## Constraints

Preserve provider order, caps, credentials, review decisions, and streamed findings.
Do not treat configured list-price estimates as provider invoices or account balances.
Do not add current-run totals to a history snapshot that already includes those requests.
Keep reporting periods independent of quota admission limits and support the existing rolling, fixed, and cron window types.
The user has not yet selected runtime period values. Keep that configuration decision pending rather than inventing billing dates.

## Review focus

Requests crossing a period boundary must retain the existing admission-time accounting rule.
Partial retained history and unpriced routed models must not appear as complete billing totals.
Unknown periods, history failures, or failed current-run report delivery must not appear as zero usage.
Reporting failures must not discard completed reviews or change provider admission.
Repeated snapshots must replace previous totals rather than accumulate them.

## Task 1: Load provider period snapshots

Modify provider configuration and the model client to accept an optional `usage_period` using `quota.Window`. Validate it independently of token caps. Add a typed provider/model key and period snapshot to the review accounting contract. Use `quota.Client.Query` after the current response's synchronous usage report completes. Reuse cache-aware pricing for the SDK-reported model and service tier. Preserve unknown prices and incomplete history explicitly.

Expose an optional model interface for loading snapshots. Invoke it once per represented provider/model at final successful, incomplete, or failed report publication. Keep network access outside renderers and recorder getters. Record current-run report delivery failures. Display unavailable period data for each affected provider/model.

- [ ] Implement configuration, typed snapshots, pricing, and publication integration.
- [ ] Verify isolation by provider/model, boundaries, deduplication, current-response inclusion, and unavailable data through public boundaries with real accounting storage.
- [ ] Commit the source changes.

## Task 2: Render the configured column

Extend `provider_details` with a `period_usage` column and enable it in the shipped presentation configuration. Render total tokens and estimated dollars with the period range and snapshot time. Use compact decimal units such as `1k`, `1m`, and `1b`; preserve useful precision near unit boundaries and for small dollar amounts. Display incomplete history and pricing coverage explicitly.

- [ ] Implement the column using typed snapshots, without adding history totals to review totals.
- [ ] Verify actual rendered Review details through the public renderer, including partial and missing data.
- [ ] Document configuration and commit the changes.

## Task 3: Verify and publish

- [ ] Resolve the pending runtime billing-period choice and configure the approved periods.
- [ ] Run the relevant Go tests and existing build gates.
- [ ] Review the complete diff for incorrect billing claims, duplicated usage, unbounded queries, and publication regressions.
- [ ] Create a ready pull request.
