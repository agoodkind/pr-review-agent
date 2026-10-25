# Provider usage and quota reference

A provider quota limits reported usage for one provider ID and configured model. The Go quota client reads usage before each model request and reports actual API usage to Cloudflare storage afterward. The client reserves no tokens. An admitted completion or concurrent requests can exceed the limit; the client denies later requests while the counted usage remains at or above the limit.

## Configuration

Provider quota fields are configured in [runtime.json](../runtime.json).

| Field | Meaning |
| --- | --- |
| `token_limit` | Maximum counted tokens within the configured window. |
| `token_types` | Count `input`, `output`, or both. Omission selects the API's reported total. |
| `token_window` | Defines the window mode and its applicable fields. |
| `token_limits` | Additional limits with independent `limit`, `token_types`, and `window` fields. Every configured limit applies to the same reported usage. |
| `daily_token_limit`, `daily_token_types` | Existing midnight UTC counters. They cannot be combined with `token_limit`, `token_types`, or `token_window`, but allow additional `token_limits`. |

Input includes cached input. Output includes reasoning tokens. A request without reported usage adds no estimate. Usage reported by an incomplete response is still counted.

A rolling window with duration `1m` limits measured tokens per minute. Additional windows can use different durations, modes, and token categories. Each response records usage once, regardless of the number of limits.

The configured model determines the counter identity. An automatic router can report a different selected model for pricing and diagnostics without changing the quota identity.

## Window modes

| Mode | Required fields | Counted admission times |
| --- | --- | --- |
| `rolling` | `duration` | After the current time minus the duration, through the current time. |
| `fixed` | `duration`, `anchor` | From the anchored interval's start, through times before its end. |
| `cron` | `schedule`, `timezone` | From the previous scheduled reset, through times before the next reset. |

Durations use Go duration notation, such as `1h30m`, `24h`, or `168h`, and require positive whole milliseconds. Fixed intervals use an RFC3339 anchor. Cron windows use five-field schedules and a named timezone, such as `America/Los_Angeles`.

Fixed durations measure elapsed time. Calendar schedules follow the selected timezone, including daylight-saving transitions. Fields for another mode are invalid.

A rolling window has no single daily reset. Its availability time is the first event expiration that reduces counted usage below the limit. Fixed and cron windows report their next boundary.

## Period usage reports

The `provider_details` list in [review-prompts.json](../config/review-prompts.json) accepts `period_usage` for the period column. Period reports reuse the existing quota windows.

The column includes each distinct period from these provider settings:

- `token_window` supplies the primary window.
- Each `token_limits[].window` supplies another window.
- `daily_token_limit` supplies the legacy UTC day.

A zero cap tracks usage for its configured period without adding an admission limit. Other positive limits still apply. Period reporting does not require a separate period setting.

Period totals count stored pr-review-agent reports across all requested models under one provider ID. Changing the configured model does not reset the provider total. Each model row for that provider displays the same period total. These totals do not include activity outside this application.

The cell labels each window and shows its reported total tokens, estimated dollars, bounds, and capture time. Pricing uses the reported model and service tier. Missing prices limit the estimate to the priced reports. Partial retained history limits both token and cost totals. The cell identifies those limits and displays unavailable data without substituting zero. Configured price estimates do not establish the provider's invoice amount.

## Attribution and history

Usage is attributed to the request's admission time. A late response retains that attribution rather than charging the interval in which it completed.

The Cloudflare quota store records admission timestamps and reported input, output, and total counts. Window and token-category changes can query this measured history. Query pages use one sequence snapshot to exclude reports inserted after the query started.

The store writes each complete SDK usage object as JSON text with its API, reported model, and response identifier. OpenAI adapters call SDK `RawJSON`, which returns unknown fields and explicit nulls while leaving omitted fields absent. Gemini adapters serialize the final cumulative SDK usage metadata; the Gemini SDK cannot expose unknown wire fields or distinguish an omitted scalar from zero. Each report counts the final cumulative snapshot once. The Worker writes and reads the supplied JSON text without changing its numbers.

The SQL history stores the complete SDK object once. Duplicate detection stores a SHA-256 digest of that object. A bounded background scan converts existing duplicate SDK objects into digests without changing SQL history. The scan stops after migration completes when retention cleanup is disabled.

`PROVIDER_USAGE_MAX_BYTES` bounds each complete report request, and `PROVIDER_USAGE_PAGE_BYTES` bounds each encoded history page. The page budget must exceed the report budget and fit the Go client's response limit. The client reads successive pages with the original sequence snapshot; native usage details do not change the canonical quota counters.

Existing daily counters remain authoritative for legacy daily quotas. The daily totals do not contain timestamps or token-category splits and are not converted into invented events. Timestamped collection begins when the quota store receives its first query or report for a provider. Diagnostics identify the measured history boundary when it does not cover the requested span.

The app's quota is independent of provider API or subscription limits. An app denial, provider quota refusal, or provider rate limit can select the next configured fallback without repeating the refused request. The `daily_budget` failure-appearance key applies to app quota denials for every window mode. The `rate_limited` key applies to provider throttling.

The [OpenAI rate-limit calculation](https://developers.openai.com/api/docs/guides/rate-limits) uses the larger of the output limit and a prompt-size estimate for admission.

## Retention

`PROVIDER_USAGE_RETENTION` configures storage cleanup independently of token limits.

| Field | Meaning |
| --- | --- |
| `max_age` | Elapsed duration of retained usage history. `0s` disables deletion. |
| `cleanup_interval` | Positive elapsed duration between background cleanup batches. |
| `batch_size` | Maximum records processed per SQL or key prefix scan in each batch, from 1 through 10000. |

Cleanup retains at least the longest configured rolling or fixed quota duration and the current legacy daily accounting window. A configured cron quota requires disabled retention cleanup because the Worker does not calculate calendar reset boundaries.

Each batch removes expired event rows, cache and service-tier details, native SDK records, duplicate-detection records, and old legacy counters. A persistent admission-time boundary rejects later reports for removed history with HTTP 410. Disabling cleanup afterward does not remove that boundary or restore deleted records. History queries report the retained-history boundary, and the Go client rejects a history boundary change during pagination.

An enabled cleanup schedule processes a bounded batch at each interval. A backlog can retain records beyond their configured age until subsequent batches remove them. Database allocation can remain larger than retained payloads after deletion because SQLite reuses freed pages.
