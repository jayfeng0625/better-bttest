#!/usr/bin/env bash
# Run the parity cases against a real Bigtable table and against the emulator built from this checkout, and diff the
# results. It needs Application Default Credentials with write access to the table, so CI cannot run it.
#
# Usage:
#   PARITY_PROJECT=<project> PARITY_INSTANCE=<instance> PARITY_TABLE=<table> \
#   PARITY_AGG_FAMILY=<int64 MIN aggregate family> PARITY_PLAIN_FAMILY=<family with no value type> \
#   scripts/parity/run.sh
#
# Exits 0 when every case matches, 1 on any difference, and 2 when the login or the target is not usable.
set -euo pipefail

for var in PARITY_PROJECT PARITY_INSTANCE PARITY_TABLE PARITY_AGG_FAMILY PARITY_PLAIN_FAMILY; do
    if [ -z "${!var:-}" ]; then
        echo "Set $var. See the usage at the top of $0." >&2
        exit 2
    fi
done
target=("$PARITY_PROJECT" "$PARITY_INSTANCE" "$PARITY_TABLE")
families=("$PARITY_AGG_FAMILY" "$PARITY_PLAIN_FAMILY")

cd "$(dirname "$0")"

if ! gcloud auth application-default print-access-token >/dev/null 2>&1; then
    echo "Application Default Credentials cannot mint a token. Log in again: gcloud auth application-default login" >&2
    exit 2
fi

npm ci --silent --no-audit --no-fund
node target.mjs "${target[@]}" "${families[@]}"

work=$(mktemp -d)
emulator_pid=
cleanup() {
    status=$?
    set +e
    if [ -n "$emulator_pid" ]; then kill "$emulator_pid" 2>/dev/null; fi
    node cleanup.mjs "${target[@]}" || status=1
    rm -rf "$work"
    exit "$status"
}
trap cleanup EXIT
trap "exit 130" INT TERM

(cd ../.. && go build -o "$work/emulator" ./cmd/emulator)
port=$(node -e 'const s = require("net").createServer().listen(0, () => { console.log(s.address().port); s.close() })')
"$work/emulator" -host localhost -port "$port" >"$work/emulator.log" 2>&1 &
emulator_pid=$!
on_emulator() { BIGTABLE_EMULATOR_HOST="localhost:$port" "$@"; }

# Same names as the real table, so the status messages that embed the table path compare exactly. The emulator
# collects garbage every second or so and production does it lazily, so both families keep every version.
cbt_emulator() { on_emulator cbt -project "$PARITY_PROJECT" -instance "$PARITY_INSTANCE" "$@"; }
for _ in $(seq 50); do
    cbt_emulator ls >/dev/null 2>&1 && break
    sleep 0.1
done
cbt_emulator createtable "$PARITY_TABLE" "families=${PARITY_AGG_FAMILY}:never:intmin,${PARITY_PLAIN_FAMILY}:never"

node cases.mjs "$work/real.json" "${target[@]}" "${families[@]}"

# A client retries a dead emulator for minutes, so stop the cases when it exits. The comparison then shows the
# cases that did not finish.
on_emulator node cases.mjs "$work/emulator.json" "${target[@]}" "${families[@]}" &
cases_pid=$!
while kill -0 "$cases_pid" 2>/dev/null; do
    if ! kill -0 "$emulator_pid" 2>/dev/null; then
        kill "$cases_pid"
        echo "The emulator exited during the run:" >&2
        grep -m1 -A6 '^panic' "$work/emulator.log" >&2 || tail -20 "$work/emulator.log" >&2
        break
    fi
    sleep 1
done
wait "$cases_pid" || true
node compare.mjs "$work/real.json" "$work/emulator.json"
