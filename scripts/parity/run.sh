#!/usr/bin/env bash
# Run the parity cases against a real Bigtable table, the emulator image built from this checkout, and Google's stock
# emulator, and diff each emulator's results against the real table's. The table cases create tables in the real
# table's instance, and delete them.
# CONTRIBUTING.md gives the usage and the exit codes.
set -euo pipefail
trap "exit 130" INT TERM

for var in PARITY_PROJECT PARITY_INSTANCE PARITY_TABLE PARITY_MIN_FAMILY PARITY_MAX_FAMILY PARITY_SUM_FAMILY \
    PARITY_PLAIN_FAMILY; do
    if [ -z "${!var:-}" ]; then
        echo "Set $var. CONTRIBUTING.md gives the usage." >&2
        exit 2
    fi
done
target=("$PARITY_PROJECT" "$PARITY_INSTANCE" "$PARITY_TABLE")
aggregates=("$PARITY_MIN_FAMILY" "$PARITY_MAX_FAMILY" "$PARITY_SUM_FAMILY")

# The smallest gcloud image that ships the Bigtable emulator.
STOCK_IMAGE=gcr.io/google.com/cloudsdktool/google-cloud-cli:emulators

cd "$(dirname "$0")"

if ! gcloud auth application-default print-access-token >/dev/null 2>&1; then
    echo "Application Default Credentials cannot mint a token. Log in again: gcloud auth application-default login" >&2
    exit 2
fi

npm ci --silent --no-audit --no-fund
node target.mjs "${target[@]}" "${aggregates[@]}" "$PARITY_PLAIN_FAMILY"

work=$(mktemp -d)
container=
cases_pid=
cleanup() {
    status=$?
    set +e
    if [ -n "$cases_pid" ]; then kill "$cases_pid" 2>/dev/null; fi
    if [ -n "$container" ]; then docker rm -f "$container" >/dev/null; fi
    node cleanup.mjs "${target[@]}" || status=1
    rm -rf "$work"
    exit "$status"
}
trap cleanup EXIT

cbt_emulator() { BIGTABLE_EMULATOR_HOST=$host cbt -project "$PARITY_PROJECT" -instance "$PARITY_INSTANCE" "$@"; }

# Start an emulator container on a free local port, then create the table with the same names as the real table, so
# the status messages that embed the table path compare exactly. The emulators collect garbage every second or so and
# production does it lazily, so every family keeps every version.
# Usage: start_emulator <image> [<command>...]. Sets container and host.
start_emulator() {
    container=$(docker run -d -p 127.0.0.1::8086 "$@")
    host=$(docker port "$container" 8086/tcp)
    for _ in $(seq 100); do
        cbt_emulator ls >/dev/null 2>&1 && break
        sleep 0.2
    done
    local families="${PARITY_MIN_FAMILY}:never:intmin,${PARITY_MAX_FAMILY}:never:intmax"
    families+=",${PARITY_SUM_FAMILY}:never:intsum,${PARITY_PLAIN_FAMILY}:never"
    cbt_emulator createtable "$PARITY_TABLE" "families=$families"
}

# Run one cases script on a fresh emulator from the image and command in emulator. A client retries a dead emulator
# for minutes, so stop the cases when it exits. The comparison then shows the cases that did not finish.
# Usage: cases_on_fresh_emulator <name> <label> <script> [<arg>...]
cases_on_fresh_emulator() {
    start_emulator "${emulator[@]}"
    BIGTABLE_EMULATOR_HOST=$host node "${@:3}" &
    cases_pid=$!
    while kill -0 "$cases_pid" 2>/dev/null; do
        if [ "$(docker inspect -f '{{.State.Running}}' "$container")" != true ]; then
            kill "$cases_pid"
            echo "The $1 emulator exited during $2:" >&2
            log=$(docker logs "$container" 2>&1)
            # gcloud prefixes each line of the stock emulator's log with [bigtable].
            grep -m1 -A6 'panic' <<<"$log" >&2 || tail -20 <<<"$log" >&2
            break
        fi
        sleep 1
    done
    wait "$cases_pid" || true
    cases_pid=
    docker rm -f "$container" >/dev/null
    container=
}

# Run the cases for each aggregate family, then the table cases, each on a fresh emulator, so a crash in one set of
# cases leaves the next to run.
# Usage: cases_on_emulator <name> <image> [<command>...]
cases_on_emulator() {
    echo "$1 emulator: $(docker image inspect -f '{{if .RepoDigests}}{{index .RepoDigests 0}}{{else}}{{.Id}}{{end}}' "$2")"
    emulator=("${@:2}")
    mkdir -p "$work/$1"
    for family in "${aggregates[@]}"; do
        cases_on_fresh_emulator "$1" "the $family cases" \
            cases.mjs "$work/$1/$family.json" "${target[@]}" "$family" "$PARITY_PLAIN_FAMILY"
    done
    cases_on_fresh_emulator "$1" "the table cases" table-cases.mjs "$work/$1/tables.json" "$PARITY_PROJECT" "$PARITY_INSTANCE"
}

docker pull -q "$STOCK_IMAGE" >/dev/null
image=$(docker build -q ../..)
cases_on_emulator better-bttest "$image"
cases_on_emulator stock "$STOCK_IMAGE" gcloud beta emulators bigtable start --host-port=0.0.0.0:8086

mkdir -p "$work/real"
for family in "${aggregates[@]}"; do
    node cases.mjs "$work/real/$family.json" "${target[@]}" "$family" "$PARITY_PLAIN_FAMILY"
done
node table-cases.mjs "$work/real/tables.json" "$PARITY_PROJECT" "$PARITY_INSTANCE"

node compare.mjs "$work" better-bttest stock
