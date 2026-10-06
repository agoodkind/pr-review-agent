# Inspect service diagnostics

Run [pragent-ops](../cmd/pragent-ops/main.go) from the repository root to audit reviews, collect logs, inspect counters, or test one provider. Go and an authenticated GitHub CLI must be installed for review audits.

## Audit failures and unresolved reviews

1. Collect the desired interval and current GitHub review state:

   ```bash
   go run ./cmd/pragent-ops stats --since 24h \
       --account-token-file /path/to/account-token
   ```

   Use `--owner` to select another repository owner. The default is the authenticated GitHub account. Use `--exclude owner/repository#number` to exclude a test pull request from the report.
2. Read the printed tables and private report. The report separates provider attempts, delivery identifiers, executions, incomplete coverage, standing reviews, and unresolved findings.
3. Inspect the evidence identifiers for each review without a recorded reassessment. A later trigger establishes an opportunity to reassess. A returned thread decision or validated verdict establishes reassessment. An unresolved finding can remain valid after reassessment.

Use `--input /path/to/capture/events.ndjson` to reuse a saved Cloudflare capture while refreshing GitHub state. Use `--operator-token-file /path/to/operator-token` to include current app quota counters. The report records the requested interval and each source's capture time.

GitHub state reflects the capture time. Cloudflare telemetry can omit earlier events within the requested interval. The report distinguishes missing evidence from a measured zero. A provider failure during fallback does not establish that the review failed. Historical rejecting reviews do not establish a standing rejection after a newer approval.

## Collect historical logs

1. Provide a file containing a Cloudflare token with Workers Observability Read permission. An account token that can create scoped tokens is also supported.
2. Collect the desired interval:

   ```bash
   go run ./cmd/pragent-ops logs --since 7d \
       --account-token-file /path/to/account-token
   ```

   Use `--token-file` for an existing scoped token. The command revokes only the temporary credential it creates from an account token.
3. Read the printed capture directory. The command saves event data, page responses, and a manifest in private files. Standard output contains counts, time bounds, credential cleanup status, and artifact paths.

Use `--from` and `--end` with RFC3339 timestamps for a fixed investigation interval. Use `--script` and `--account` to override the Worker and account selected from the Wrangler configuration. Use `--output-dir` to select a new private destination.

The command pages until Cloudflare returns a validated empty event array. It deduplicates overlapping page boundaries. A failed request, malformed response, nonadvancing boundary, or page safety limit fails the capture. A short page does not establish completion.

The manifest records whether the available telemetry was exhausted. It does not establish that Cloudflare retained every event the service generated. Preserve the requested interval and observed record bounds when comparing logs with GitHub checks.

Raw provider errors can contain credentials or request content. Inspect the private capture instead of copying those values into comments or command output.

## Summarize a saved capture

1. Use the event file from a completed capture:

   ```bash
   go run ./cmd/pragent-ops metrics \
       --input /path/to/capture/events.ndjson
   ```
2. Read the printed metrics artifact. The command performs no Cloudflare requests when `--input` is supplied.
3. Compare distinct run identifiers with GitHub check runs. Keep provider attempts separate from final incomplete reviews.

The summary distinguishes app quota denials, provider API exhaustion, and unclassified failures. New provider-attempt records include structured provider, model, quota, and failure fields. Older records may require recognition of the service's historical error wording. Missing token measurements remain unknown rather than zero.

## Inspect current provider counters

1. Register a Cloudflare credential file as the operator token:

   ```bash
   go run ./cmd/pragent-ops operator-token \
       --account-token-file /path/to/account-token
   ```

   The command uploads only the token's SHA256 digest as `OPERATOR_TOKEN_SHA256`. It creates a temporary account-scoped token with Workers Scripts Write permission, then revokes that temporary token and verifies rejection. Use `--token-file` instead for an existing credential with that permission. Use `--operator-token-file` to register a separate credential. Registering another token replaces the previous operator credential.
2. Query the configured quota endpoint:

   ```bash
   go run ./cmd/pragent-ops counters \
       --runtime runtime.json \
       --operator-token-file /path/to/account-token
   ```
3. Read the printed counter artifact. Use `--provider` with an exact configured provider ID to inspect one provider.

The operator token permits counter reads. Usage reports and reservations require the existing webhook signature. The CLI also accepts `--signing-key-file` for signed counter reads; supply only one authentication file.

Provider API balances and subscription allowances are separate from the app's counters. Counter reports include the measured time bounds; newly collected timestamped history cannot reconstruct earlier daily totals.

## Test one provider

1. Provide a private file containing the selected provider's existing API credential.
2. Send one review request using its configured model, API, output limit, and structured-response format:

   ```bash
   go run ./cmd/pragent-ops probe --provider gemini \
       --credential-file /path/to/provider-key
   ```

   Use `--prompt-file` to test a saved review input. The command uses the production client with one selected provider and performs no retries or fallback. It preserves configured app quotas; capped providers also require `--signing-key-file`.
3. Inspect the printed private output directory. SDK logs and error response bodies remain in private files. Standard output contains the configured provider, model, request outcome, and artifact paths.

## Watch new events

1. Authenticate Wrangler for Workers Tail access.
2. Run the live tail from the [Cloudflare deployment directory](../deploy/cloudflare):

   ```bash
   npx wrangler tail agoodkind-nano-pr-reviewer --format json
   ```

Live tail starts with new events. Use historical collection for earlier runs.
