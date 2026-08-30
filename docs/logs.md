# Read the service logs

The review service ships every line it writes to the Worker, which prints them,
so they land in Workers Logs. Nothing else reads them back. This page is how you
pull them, and it is written so an agent can repeat it without guessing.

## What you can and cannot get

Cloudflare retains Workers Logs for 7 days on the Workers Paid plan. Paging to
exhaustion on 2026-08-29 returned 1387 events spanning 2026-08-22T18:20:15Z to
2026-08-29T17:22:17Z. Nothing older exists to retrieve, so a question about a run
from three weeks ago has no answer from this source.

The API caps one response at 2000 events. Asking for more returns a validation
error naming `limit` and `too_big`.

A short response does not mean you reached the end. The first page of that
2026-08-29 dump returned 1385 events, well under the cap, and a second page still
found 3 more. Page until a request returns zero.

## Get a credential

Reading logs needs an API token carrying the **Workers Observability Read**
permission. The account OAuth token that `wrangler` stores does not carry it, and
`wrangler` has no historical log command at all, so both return an authentication
error numbered 10000.

Mint a scoped token from the account token kept at `~/Desktop/cftoken/token.txt`.
It carries one permission group, Workers Observability Read, because that is all
the queries below use. It also expires on its own after a day, so a skipped or
failed revocation cannot leave an account wide credential alive indefinitely.

A bearer token passed with `-H` is visible in the process arguments to every
other user on the host, so pass it through a curl config file instead. Write
the config with `umask 077` so only you can read it.

```bash
cd ~/Desktop/cftoken
umask 077
printf 'header = "Authorization: Bearer %s"\n' "$(<token.txt)" > account.conf
curl -sS --fail-with-body --config account.conf \
    -X POST \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"pr-agent-observability-read\",\"expires_on\":\"$(date -u -v+1d +%Y-%m-%dT%H:%M:%SZ)\",\"policies\":[{\"effect\":\"allow\",\"permission_groups\":[{\"id\":\"66c1ed49f4ed46098b75696a6d4ee3c9\"}],\"resources\":{\"com.cloudflare.api.account.ee7d7ca7d611ef8c2a07885e8362de0c\":\"*\"}}]}" \
    "https://api.cloudflare.com/client/v4/user/tokens" > created.json
```

Check that it worked before writing anything. A failed call still returns JSON,
and `jq -r` would write the string `null` into the token file, which then fails
later with a confusing authentication error instead of an obvious one.

```bash
jq -e '.success == true and (.result.value | type == "string") and (.result.id | type == "string")' created.json > /dev/null || {
    echo "token creation failed:" >&2
    jq '.errors' created.json >&2
    exit 1
}
jq -r '.result.value' created.json > observability.txt
jq -r '.result.id' created.json > observability.id
printf 'header = "Authorization: Bearer %s"\n' "$(<observability.txt)" > observability.conf
rm created.json
chmod 600 observability.txt observability.id observability.conf account.conf
```

The value now sits in `~/Desktop/cftoken/observability.txt`, and every command
below reads it through `observability.conf`. Never print either one.

Revoke it when you are done, using the identifier you saved. Check the result:
a silently failed revocation leaves a privileged non expiring token alive.

```bash
cd ~/Desktop/cftoken
curl -sS --fail-with-body --config account.conf \
    -X DELETE \
    "https://api.cloudflare.com/client/v4/user/tokens/$(<observability.id)" > revoked.json
jq -e '.success == true' revoked.json > /dev/null && rm -f observability.txt observability.id observability.conf revoked.json || {
    echo "revocation failed, the token is still live:" >&2
    jq '.errors' revoked.json >&2
}
rm -f account.conf
```

## Dump one page

One request. `timeframe.from` and `timeframe.to` are milliseconds since the
epoch, and the wide window here asks for everything so retention decides.

Compute the upper bound from the clock rather than hardcoding one. A fixed
timestamp silently drops every log written after it, which is the failure you
would least notice: the query still succeeds and just returns less.

```bash
NOW_MS=$(( $(date +%s) * 1000 ))
jq -n --argjson to "$NOW_MS" '{queryId:"dump",timeframe:{from:0,to:$to},parameters:{datasets:["cloudflare-workers"]},limit:2000,view:"events"}' > req.json
curl -sS --fail-with-body --config ~/Desktop/cftoken/observability.conf \
    -X POST \
    -H "Content-Type: application/json" \
    --data @req.json \
    "https://api.cloudflare.com/client/v4/accounts/ee7d7ca7d611ef8c2a07885e8362de0c/workers/observability/telemetry/query" \
    > page-1.json
```

A `from` of 0 asks for everything, so retention decides the real start.

That returns the newest events first and stops. It is one page, not the whole
set.

## Dump everything

There is no cursor. Page backwards through time: take the oldest timestamp a
page returned and make it the next page's `to`. Stop when a page returns zero.

Check the request and the response envelope before trusting that zero. A failed
call still writes a JSON error body, whose event count reads as zero, which the
loop would otherwise treat as a clean finish and hand you a partial dump that
looks complete.

```bash
URL="https://api.cloudflare.com/client/v4/accounts/ee7d7ca7d611ef8c2a07885e8362de0c/workers/observability/telemetry/query"
TO=$(( $(date +%s) * 1000 ))

for i in $(seq 1 40); do
    jq -n --argjson to "$TO" '{queryId:"page",timeframe:{from:0,to:$to},parameters:{datasets:["cloudflare-workers"]},limit:2000,view:"events"}' > req.json
    if ! curl -sS --fail-with-body --config ~/Desktop/cftoken/observability.conf \
        -X POST -H "Content-Type: application/json" --data @req.json "$URL" > "page-$i.json"; then
        echo "page $i request failed, the dump is incomplete" >&2
        rm -f "page-$i.json"
        exit 1
    fi
    if ! N=$(jq -er 'select(.success == true) | .result.events.events | arrays | length' "page-$i.json"); then
        echo "page $i returned no event array, the dump is incomplete" >&2
        exit 1
    fi
    echo "page $i: $N events"
    if [[ "$N" -eq 0 ]]; then rm -f "page-$i.json"; break; fi
    TO=$(jq '.result.events.events | min_by(.timestamp).timestamp' "page-$i.json")
done
```

Merge the pages into one array. The boundary timestamp repeats across pages, so
deduplicate on the record identifier:

```bash
jq -s '[.[].result.events.events[]] | unique_by(.timestamp, .["$metadata"].id)' page-*.json > all-events.json
jq 'length' all-events.json
jq -r '(min_by(.timestamp).timestamp/1000|todate) + " to " + (max_by(.timestamp).timestamp/1000|todate)' all-events.json
```

Every query below reads `all-events.json`, which is a bare array rather than the
API envelope.

## Read one record

The array mixes the Worker's own lines with the service's. A service record
carries `source: "pr-review-agent"` and its fields sit under `.source`:

```bash
jq 'map(select(.source.source=="pr-review-agent")) | .[0].source' all-events.json
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
jq -r '.[].source | select(.level=="ERROR") | (.time + "  " + .repository + " #" + .pull_request + "  " + .message + "  " + .err)' all-events.json
```

Follow one run:

```bash
jq -r --arg run ce224b60-a3b8-11f1-8990-692ed34d72f4 '.[].source | select(.delivery_id==$run) | (.time + "  " + .level + "  " + .message)' all-events.json
```

Count failures per repository:

```bash
jq -r '[.[].source | select(.level=="ERROR") | .repository] | group_by(.) | map({repo: .[0], n: length}) | sort_by(-.n) | .[] | "\(.n)  \(.repo)"' all-events.json
```

## Narrow the request instead

Add a filter when the full dump is more than you want. `operation` accepts
`includes`, and `key` names any field on the record:

```bash
NOW_MS=$(( $(date +%s) * 1000 ))
DAY_AGO_MS=$(( NOW_MS - 86400000 ))
jq -n --argjson from "$DAY_AGO_MS" --argjson to "$NOW_MS" '{queryId:"errors",timeframe:{from:$from,to:$to},parameters:{datasets:["cloudflare-workers"],filters:[{key:"$metadata.message",operation:"includes",value:"chunk request failed",type:"string"}]},limit:200,view:"events"}' > req.json
curl -sS --fail-with-body --config ~/Desktop/cftoken/observability.conf \
    -X POST -H "Content-Type: application/json" --data @req.json \
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
