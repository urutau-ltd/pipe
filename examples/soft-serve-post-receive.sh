#!/bin/sh
set -eu

# Minimal but robust post-receive hook for soft-serve + pipe.
#
# Features:
# - branch->pipelines mapping via env vars
# - non-blocking push by default (PIPE_WAIT=0)
# - optional status polling with /runs endpoint (PIPE_WAIT=1)
#
# Install:
#   cp examples/soft-serve-post-receive.sh /opt/containers/soft-serve/hooks/post-receive
#   chmod +x /opt/containers/soft-serve/hooks/post-receive

PIPE_ENDPOINT="${PIPE_ENDPOINT:-http://pipe:9000}"
PIPE_WAIT="${PIPE_WAIT:-0}"
PIPE_WAIT_INTERVAL="${PIPE_WAIT_INTERVAL:-2}"
PIPE_WAIT_TIMEOUT="${PIPE_WAIT_TIMEOUT:-1800}"

# Per-branch defaults. Override with environment variables if needed.
PIPELINES_MAIN="${PIPELINES_MAIN:-[\"ci\",\"release\"]}"
PIPELINES_NIGHTLY="${PIPELINES_NIGHTLY:-[\"nightly\"]}"
PIPELINES_DEFAULT="${PIPELINES_DEFAULT:-[\"ci\"]}"

http_post_json() {
    url="$1"
    payload="$2"

    if command -v curl >/dev/null 2>&1; then
        curl -fsS -X POST "$url" \
            -H "Content-Type: application/json" \
            -d "$payload"
        return 0
    fi

    if command -v wget >/dev/null 2>&1; then
        wget -q -O - \
            --header="Content-Type: application/json" \
            --post-data="$payload" \
            "$url"
        return 0
    fi

    echo "pipe hook: curl or wget is required" >&2
    return 127
}

http_get() {
    url="$1"

    if command -v curl >/dev/null 2>&1; then
        curl -fsS "$url"
        return 0
    fi

    if command -v wget >/dev/null 2>&1; then
        wget -q -O - "$url"
        return 0
    fi

    echo "pipe hook: curl or wget is required" >&2
    return 127
}

select_pipelines() {
    ref="$1"
    case "$ref" in
        refs/heads/main)
            printf '%s' "$PIPELINES_MAIN"
            ;;
        refs/heads/nightly)
            printf '%s' "$PIPELINES_NIGHTLY"
            ;;
        refs/heads/*)
            printf '%s' "$PIPELINES_DEFAULT"
            ;;
        *)
            printf ''
            ;;
    esac
}

extract_run_ids() {
    response="$1"
    printf '%s\n' "$response" | sed -n 's/.* runs=\([^ ]*\).*/\1/p'
}

extract_status() {
    body="$1"
    if command -v jq >/dev/null 2>&1; then
        printf '%s\n' "$body" | jq -r '.status // empty'
        return 0
    fi
    printf '%s\n' "$body" | sed -n 's/.*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1
}

wait_for_run() {
    run_id="$1"
    started="$(date +%s)"
    deadline="$((started + PIPE_WAIT_TIMEOUT))"

    while :; do
        now="$(date +%s)"
        if [ "$now" -ge "$deadline" ]; then
            echo "pipe hook: timeout waiting for run $run_id" >&2
            return 1
        fi

        body="$(http_get "${PIPE_ENDPOINT}/runs?id=${run_id}" 2>/dev/null || true)"
        status="$(extract_status "$body")"

        case "$status" in
            ok)
                echo "pipe hook: run $run_id finished: ok"
                return 0
                ;;
            fail|ignored)
                echo "pipe hook: run $run_id finished: $status"
                echo "pipe hook: log ${PIPE_ENDPOINT}/runs/log?id=${run_id}"
                return 1
                ;;
            queued|running|"")
                sleep "$PIPE_WAIT_INTERVAL"
                ;;
            *)
                echo "pipe hook: run $run_id unknown status: $status"
                sleep "$PIPE_WAIT_INTERVAL"
                ;;
        esac
    done
}

while read -r OLD NEW REF; do
    case "$REF" in
        refs/heads/*) ;;
        *)
            echo "pipe hook: ignoring non-branch ref $REF"
            continue
            ;;
    esac

    REPO="$(basename "$PWD" .git)"
    PIPELINES="$(select_pipelines "$REF")"
    if [ -z "$PIPELINES" ]; then
        PAYLOAD="$(printf '{"repo":"%s","ref":"%s","old":"%s","new":"%s"}' \
            "$REPO" "$REF" "$OLD" "$NEW")"
    else
        PAYLOAD="$(printf '{"repo":"%s","ref":"%s","old":"%s","new":"%s","pipelines":%s}' \
            "$REPO" "$REF" "$OLD" "$NEW" "$PIPELINES")"
    fi

    RESPONSE="$(http_post_json "${PIPE_ENDPOINT}/run" "$PAYLOAD" 2>/dev/null || true)"
    if [ -z "$RESPONSE" ]; then
        echo "pipe hook: failed to enqueue repo=$REPO ref=$REF endpoint=${PIPE_ENDPOINT}/run" >&2
        continue
    fi

    echo "pipe hook: $RESPONSE"
    RUN_IDS="$(extract_run_ids "$RESPONSE")"
    if [ "$PIPE_WAIT" != "1" ] || [ -z "$RUN_IDS" ]; then
        continue
    fi

    OLD_IFS="$IFS"
    IFS=','
    for RUN_ID in $RUN_IDS; do
        wait_for_run "$RUN_ID" || true
    done
    IFS="$OLD_IFS"
done
