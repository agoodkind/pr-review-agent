# Read logs, metrics, and quota counters

Run [pragent-ops](../cmd/pragent-ops/main.go) from the repository root to collect Cloudflare logs, summarize saved records, or inspect provider counters. Go must be installed.

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

1. Provide a private file containing the existing `GITHUB_WEBHOOK_SECRET`. Cloudflare's [secret metadata API](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/secrets/methods/get/) does not return its value.
2. Query the configured quota endpoint:

   ```bash
   go run ./cmd/pragent-ops counters \
       --runtime runtime.json \
       --signing-key-file /path/to/webhook-signing-key
   ```
3. Read the printed counter artifact. Use `--provider` with an exact configured provider ID to inspect one provider.

The command uses the production signed quota client. Provider API balances and subscription allowances are separate from the app's counters. Counter reports include the measured time bounds; newly collected timestamped history cannot reconstruct earlier daily totals.

## Watch new events

1. Authenticate Wrangler for Workers Tail access.
2. Run the live tail from the [Cloudflare deployment directory](../deploy/cloudflare):

   ```bash
   npx wrangler tail agoodkind-nano-pr-reviewer --format json
   ```

Live tail starts with new events. Use historical collection for earlier runs.
