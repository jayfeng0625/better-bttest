#!/usr/bin/env bash
# Run the parity cases against a real Bigtable table, the emulator image built from this checkout, and Google's stock
# emulator, and diff each emulator's results against the real table's. README.md gives the usage and the exit codes.
set -euo pipefail

for var in PARITY_PROJECT PARITY_INSTANCE PARITY_TABLE PARITY_MIN_FAMILY PARITY_MAX_FAMILY PARITY_PLAIN_FAMILY; do
    if [ -z "${!var:-}" ]; then
        echo "Set $var. README.md gives the usage." >&2
        exit 2
    fi
done
target=("$PARITY_PROJECT" "$PARITY_INSTANCE" "$PARITY_TABLE")
aggregates=("$PARITY_MIN_FAMILY" "$PARITY_MAX_FAMILY")

# Google's emulator as gcloud ships it.
STOCK_IMAGE=gcr.io/google.com/cloudsdktool/google-cloud-cli:latest

cd "$(dirname "$0")"

if ! gcloud auth application-default print-access-token >/dev/null 2>&1; then
    echo "Application Default Credentials cannot mint a token. Log in again: gcloud auth application-default login" >&2
    exit 2
fi

npm ci --silent --no-audit --no-fund
node target.mjs "${target[@]}" "${aggregates[@]}" "$PARITY_PLAIN_FAMILY"

work=$(mktemp -d)
containers=
cleanup() {
    status=$?
    set +e
    if [ -n "$containers" ]; then docker rm -f $containers >/dev/null; fi
    node cleanup.mjs "${target[@]}" || status=1
    rm -rf "$work"
    exit "$status"
}
trap cleanup EXIT
trap "exit 130" INT TERM

# Start an emulator container on a free local port, then create the table with the same names as the real table, so
# the status messages that embed the table path compare exactly. The emulators collect garbage every second or so and
# production does it lazily, so every family keeps every version.
# Usage: start_emulator <image> [<command>...]. Sets container and host.
start_emulator() {
    container=$(docker run -d -p 127.0.0.1::8086 "$@")
    containers="$containers $container"
    host=$(docker port "$container" 8086/tcp)
    for _ in $(seq 100); do
        BIGTABLE_EMULATOR_HOST=$host cbt -project "$PARITY_PROJECT" -instance "$PARITY_INSTANCE" ls >/dev/null 2>&1 && break
        sleep 0.2
    done
    BIGTABLE_EMULATOR_HOST=$host cbt -project "$PARITY_PROJECT" -instance "$PARITY_INSTANCE" createtable "$PARITY_TABLE" \
        "families=${PARITY_MIN_FAMILY}:never:intmin,${PARITY_MAX_FAMILY}:never:intmax,${PARITY_PLAIN_FAMILY}:never"
}

running() { [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" = true ]; }

# Run the cases for each aggregate family on a fresh emulator, so a crash in one family's cases leaves the next
# family's cases to run. A client retries a dead emulator for minutes, so stop the cases when it exits. The comparison
# then shows the cases that did not finish.
# Usage: cases_on_emulator <name> <image> [<command>...]
cases_on_emulator() {
    echo "$1 emulator: $(docker image inspect -f '{{if .RepoDigests}}{{index .RepoDigests 0}}{{else}}{{.Id}}{{end}}' "$2")"
    mkdir -p "$work/$1"
    for family in "${aggregates[@]}"; do
        start_emulator "${@:2}"
        BIGTABLE_EMULATOR_HOST=$host node cases.mjs "$work/$1/$family.json" "${target[@]}" "$family" "$PARITY_PLAIN_FAMILY" &
        cases_pid=$!
        while kill -0 "$cases_pid" 2>/dev/null; do
            if ! running "$container"; then
                kill "$cases_pid"
                echo "The $1 emulator exited during the $family cases:" >&2
                docker logs "$container" 2>&1 | grep -m1 -A6 'panic' >&2 || docker logs --tail 20 "$container" >&2
                break
            fi
            sleep 1
        done
        wait "$cases_pid" || true
    done
}

docker pull -q "$STOCK_IMAGE" >/dev/null
cases_on_emulator better-bttest "$(docker build -q ../..)"
cases_on_emulator stock "$STOCK_IMAGE" gcloud beta emulators bigtable start --host-port=0.0.0.0:8086

mkdir -p "$work/real"
for family in "${aggregates[@]}"; do
    node cases.mjs "$work/real/$family.json" "${target[@]}" "$family" "$PARITY_PLAIN_FAMILY"
done

node compare.mjs "$work" real better-bttest stock
