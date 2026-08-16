#!/usr/bin/env bash

set -euo pipefail

HEALTH_ORIGIN="${HEALTH_ORIGIN:-https://agoodkind-nano-pr-reviewer.alex-ee7.workers.dev}"
HEALTH_URL="${HEALTH_URL:-${HEALTH_ORIGIN%/}/}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-30}"
RETRY_DELAY_SECONDS="${RETRY_DELAY_SECONDS:-10}"

for ((attempt = 1; attempt <= MAX_ATTEMPTS; attempt++)); do
    if status_code="$(curl --connect-timeout 10 --max-time 20 --silent --show-error --output /dev/null --write-out '%{http_code}' "$HEALTH_URL")"; then
        if [[ "$status_code" == "200" ]]; then
            exit 0
        fi

        echo "Health endpoint returned HTTP $status_code on attempt $attempt of $MAX_ATTEMPTS."
    else
        curl_status=$?
        echo "Health probe failed with curl exit $curl_status on attempt $attempt of $MAX_ATTEMPTS."
    fi

    sleep "$RETRY_DELAY_SECONDS"
done

echo "Health endpoint did not return HTTP 200 after $MAX_ATTEMPTS attempts."
exit 1
