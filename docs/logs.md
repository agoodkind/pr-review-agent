# Read the service logs

The review service ships every line it writes to the Worker, which prints them,
so they land in Workers Logs. Nothing else reads them back. This page is how you
pull them, and it is written so an agent can repeat it without guessing.

## What you can and cannot get

Cloudflare retains Workers Logs for 7 days on the Workers Paid plan. A dump on
2026-08-29 returned 1385 events spanning 2026-08-22 to 2026-08-29. Nothing older
exists to retrieve, so a question about a run from three weeks ago has no answer
from this source.

The API caps one response at 2000 events. Asking for more returns a validation
error naming `limit` and `too_big`.

## Get a credential

Reading logs needs an API token carrying the **Workers Observability Read**
permission. The account OAuth token that `wrangler` stores does not carry it, and
`wrangler` has no historical log command at all, so both return an authentication
error numbered 10000.

Mint a scoped token from the account token kept at `~/Desktop/cftoken/token.txt`.
The three permission group identifiers below are Workers Observability Read,
Workers Scripts Read, and Containers Read.

```bash
cd ~/Desktop/cftoken
umask 077
curl -s -X POST \
    -H "Authorization: Bearer $(<token.txt)" \
    -H "Content-Type: application/json" \
    -d '{"name":"pr-agent-observability-read","policies":[{"effect":"allow","permission_groups":[{"id":"66c1ed49f4ed46098b75696a6d4ee3c9"},{"id":"cfd39eebc07c4e3ea849e4b3d2644637"},{"id":"1a71c399035b4950a1bd1466bbe4f420"}],"resources":{"com.cloudflare.api.account.ee7d7ca7d611ef8c2a07885e8362de0c":"*"}}]}' \
    "https://api.cloudflare.com/client/v4/user/tokens" > created.json
jq -r '.result.value' created.json > observability.txt
jq -r '.result.id' created.json > observability.id
rm created.json
chmod 600 observability.txt observability.id
```

The value now sits in `~/Desktop/cftoken/observability.txt`. Never print it.

Revoke it when you are done, using the identifier you saved:

```bash
cd ~/Desktop/cftoken
curl -s -X DELETE \
    -H "Authorization: Bearer $(<token.txt)" \
    "https://api.cloudflare.com/client/v4/user/tokens/$(<observability.id)"
```

## Dump every retained log line

One request, no script:

```bash
curl -s -X POST \
    -H "Authorization: Bearer $(<~/Desktop/cftoken/observability.txt)" \
    -H "Content-Type: application/json" \
    -d '{"queryId":"dump","timeframe":{"from":1754000000000,"to":1788200000000},"parameters":{"datasets":["cloudflare-workers"]},"limit":2000,"view":"events"}' \
    "https://api.cloudflare.com/client/v4/accounts/ee7d7ca7d611ef8c2a07885e8362de0c/workers/observability/telemetry/query" \
    > alltime.json
```

`timeframe.from` and `timeframe.to` are milliseconds since the epoch. The wide
window above simply asks for everything and lets retention decide.

Count what came back and see the span it covers:

```bash
jq '.result.events.events | length' alltime.json
jq -r '.result.events.events | (min_by(.timestamp).timestamp/1000|todate) + " to " + (max_by(.timestamp).timestamp/1000|todate)' alltime.json
```

## Read one record

Each event carries the service's own fields under `.source`:

```bash
jq '.result.events.events[0].source' alltime.json
```

A failed chunk looks like this:

```json
{
  "level": "ERROR",
  "source": "pr-review-agent",
  "time": "2026-08-29T14:53:07.999380065Z",
  "message": "review chunk request failed",
  "build": "pr-review-agent 202608261257-33-4073a01 (4073a01, built 2026-08-26T12:58:10Z)",
  "chunk": "3",
  "chunks": "7",
  "delivery_id": "ce224b60-a3b8-11f1-8990-692ed34d72f4",
  "elapsed": "3m16.267540732s",
  "err": "model provider request failed before receiving a response: stream error: stream ID 21; INTERNAL_ERROR; received from peer",
  "head": "f5e5bf087fb8301e65a1fd83c47d4600800c9a4c",
  "pull_request": "154",
  "repository": "agoodkind/tack"
}
```

`delivery_id` is the GitHub webhook delivery, and it is the identifier that ties
every line of one run together.

## Answer why runs died

Filter the dump you already have rather than issuing another request:

```bash
jq -r '.result.events.events[].source | select(.level=="ERROR") | (.time + "  " + .repository + " #" + .pull_request + "  " + .message + "  " + .err)' alltime.json
```

Follow one run end to end:

```bash
jq -r --arg run ce224b60-a3b8-11f1-8990-692ed34d72f4 '.result.events.events[].source | select(.delivery_id==$run) | (.time + "  " + .level + "  " + .message)' alltime.json
```

## Narrow the request instead

Add a filter when the full dump is more than you want. `operation` accepts
`includes`, and `key` names any field on the record:

```bash
curl -s -X POST \
    -H "Authorization: Bearer $(<~/Desktop/cftoken/observability.txt)" \
    -H "Content-Type: application/json" \
    -d '{"queryId":"errors","timeframe":{"from":1787500000000,"to":1788200000000},"parameters":{"datasets":["cloudflare-workers"],"filters":[{"key":"$metadata.message","operation":"includes","value":"chunk request failed","type":"string"}]},"limit":200,"view":"events"}' \
    "https://api.cloudflare.com/client/v4/accounts/ee7d7ca7d611ef8c2a07885e8362de0c/workers/observability/telemetry/query"
```

## Watch a run happen

`wrangler tail` streams the same lines live and needs only the account OAuth
token, which already carries `workers_tail`:

```bash
cd deploy/cloudflare
npx wrangler tail agoodkind-nano-pr-reviewer --format json
```

It shows nothing that happened before you started it.

## Where this is incomplete

Following one run end to end works only partly today. Of 309 service records in
the 2026-08-29 dump, 211 carried a `delivery_id`. The 98 that did not are almost
all `http request` debug lines, plus startup and configuration lines that belong
to no run.

So a `delivery_id` filter finds a run's review work and misses the request
handling around it. Correlation identifiers stamped on every record close that
gap, and until they land, read a narrow time window around the run instead of
relying on the identifier alone.
