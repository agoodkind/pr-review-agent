# Report provider usage in configured token windows

> Use `executing-plans` to implement these tasks in dependency order.

**Goal:** Add a configurable Review details column that reports measured tokens and estimated cost across reviews for each provider.

**Architecture:** Reuse persistent quota event history and the periods already configured for token limits. Load one grouped snapshot per provider and distinct window at final publication after usage reporting completes. Include every requested model used during that period. Keep those totals separate from the current review's usage and display their time range and measurement coverage.

**Tech Stack:** Go, existing provider SDK adapters, Cloudflare accounting storage, and existing pricing configuration.

## Constraints

Preserve provider order, caps, credentials, review decisions, and streamed findings.
Do not treat configured list-price estimates as provider invoices or account balances.
Do not add current-run totals to a history snapshot that already includes those requests.
Use existing token windows, including legacy UTC daily periods and rolling, fixed, or cron windows. Display every distinct window when a provider has several limits.
Keep zero limits disabled while retaining their window configuration for reporting. Do not add a separate reporting-period setting or require billing dates.

## Review focus

Requests crossing a period boundary must retain the existing admission-time accounting rule.
Partial retained history and unpriced routed models must not appear as complete billing totals.
Unknown periods, history failures, or failed current-run report delivery must not appear as zero usage.
Reporting failures must not discard completed reviews or change provider admission.
Repeated snapshots must replace previous totals rather than accumulate them.

## Task 1: Load provider period snapshots

Derive periods from `token_window`, `token_limits[].window`, and the legacy daily limit. Remove the independent `usage_period` setting. Retain windows for zero limits without checking them during admission or requiring a quota service for admission. Validate each window using the existing rules.

Reuse the bounded grouped query over existing accounting tables after the current response's synchronous usage report completes. Preserve each group's requested model and SDK-reported model for cache-aware pricing. Keep the existing per-model quota query unchanged. Preserve unknown prices and incomplete history explicitly.

Load each distinct provider/window once at final successful, incomplete, or failed report publication. Keep network access outside renderers and recorder getters. Record current-run report delivery failures and display unavailable data for each affected provider.

## Task 2: Render the configured column

Use the existing configurable `period_usage` column. Show separate totals for every applicable quota window. Render total tokens and estimated dollars with the period range and snapshot time. Use compact decimal units such as `1k`, `1m`, and `1b`; preserve useful precision near unit boundaries and for small dollar amounts. Display incomplete history and pricing coverage explicitly.

Refresh every applicable period in preserved review details without changing the original review's statistics.

## Task 3: Verify and publish

Verify public configuration loading, multiple periods, disabled-cap admission, window boundaries, and measured totals using real accounting storage. Verify the public renderer for multiple windows, compact numbers, partial costs, unavailable history, and separation from current-review totals.

Run the relevant Go tests and existing build gates. Review the complete diff for incorrect billing claims, duplicated usage, unbounded queries, and publication regressions. Publish the correction as a ready pull request.
