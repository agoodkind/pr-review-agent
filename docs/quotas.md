# Provider token quota reference

A provider quota limits reported usage for one provider ID and configured model. The Go quota client reads usage before each model request and reports actual API usage to Cloudflare storage afterward. The client reserves no tokens. An admitted completion or concurrent requests can exceed the limit; the client denies later requests while the counted usage remains at or above the limit.

## Configuration

Provider quota fields are configured in [runtime.json](../runtime.json).

| Field | Meaning |
| --- | --- |
| `token_limit` | Maximum counted tokens within the configured window. |
| `token_types` | Count `input`, `output`, or both. Omission selects the API's reported total. |
| `token_window` | Defines the window mode and its applicable fields. |
| `daily_token_limit`, `daily_token_types` | Compatibility fields for the existing midnight UTC counters. They cannot be combined with generic quota fields. |

Input includes cached input. Output includes reasoning tokens. A request without reported usage adds no estimate. Usage reported by an incomplete response is still counted.

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

## Attribution and history

Usage is attributed to the request's admission time. A late response retains that attribution rather than charging the interval in which it completed.

The Cloudflare quota store records admission timestamps and reported input, output, and total counts. Window and token-category changes can query this measured history. Query pages use one sequence snapshot to exclude reports inserted after the query started.

Existing daily counters remain authoritative for legacy daily quotas. The daily totals do not contain timestamps or token-category splits and are not converted into invented events. Timestamped collection begins when the quota store receives its first query or report for a provider. Diagnostics identify the measured history boundary when it does not cover the requested span.

The app's quota is independent of provider API or subscription limits. Exhaustion of either can select the next configured fallback. The `daily_budget` failure-appearance key remains compatible with app quota denials for every window mode.
