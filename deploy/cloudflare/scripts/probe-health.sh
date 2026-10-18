#!/usr/bin/env bash

set -euo pipefail

HEALTH_ORIGIN="${HEALTH_ORIGIN:-https://agoodkind-nano-pr-reviewer.alex-ee7.workers.dev}"
HEALTH_URL="${HEALTH_URL:-${HEALTH_ORIGIN%/}/}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-30}"
RETRY_DELAY_SECONDS="${RETRY_DELAY_SECONDS:-10}"
EXPECTED_REVISION="${EXPECTED_REVISION:-}"

for ((attempt = 1; attempt <= MAX_ATTEMPTS; attempt++)); do
    if response_status="$(curl --connect-timeout 10 --max-time 20 --silent --show-error --output /dev/null --write-out '%{http_code} %header{x-pr-agent-revision}' "$HEALTH_URL")"; then
        status_code="${response_status%% *}"
        revision="${response_status#* }"
        if [[ "$status_code" == "200" ]]; then
            if [[ -z "$EXPECTED_REVISION" ]]; then
                exit 0
            fi
            if [[ "$revision" == "$EXPECTED_REVISION" ]]; then
                exit 0
            fi
            echo "Container revision ${revision:-missing} does not match expected $EXPECTED_REVISION on attempt $attempt of $MAX_ATTEMPTS."
        else
            echo "Health endpoint returned HTTP $status_code on attempt $attempt of $MAX_ATTEMPTS."
        fi
    else
        curl_status=$?
        echo "Health probe failed with curl exit $curl_status on attempt $attempt of $MAX_ATTEMPTS."
    fi

    sleep "$RETRY_DELAY_SECONDS"
done

echo "Health endpoint did not pass the status and configured revision checks after $MAX_ATTEMPTS attempts."
exit 1
