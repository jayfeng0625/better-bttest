# better-bttest contribution guide

## Run the checks

```sh
scripts/check.sh
```

The script runs the same checks as CI: gofmt, the license headers, `go mod tidy`, the test value encoding, `go vet`, staticcheck, and the tests under the race detector.
CI runs it on the latest Go 1.26 patch release, with `GOTOOLCHAIN=local`.

## Change an upstream file

Upstream is Google's emulator code in google-cloud-go.
An upstream file is a file that `scripts/sync-upstream.sh` copies from upstream, such as `bttest/inmem.go`.
Change upstream files as little as you can, so that the next [sync with upstream](#sync-with-upstream) merges without a conflict:

- Put new code in new files. Change an upstream file only to call the new code.
- To replace an upstream function, delete it from the upstream file.
  Define it under the same name in a new file, as `bttest/gc.go` does for `applyGC`.
  If upstream later changes the function, the sync then stops with a conflict.
- Keep upstream's style, comments, and log lines as they are. No test catches a changed log line.

## Add the license header

Start a new Go file with `// SPDX-License-Identifier: Apache-2.0`.
Keep Google's Apache-2.0 header in an upstream file.

## Test the stored bytes

Write each test input and expected value with `binary.BigEndian`, as the bytes that production stores.
Upstream's aggregate tests in `bttest/inmem_test.go` do the same.
Do not call the code's own `encodeInt64` or `decodeInt64` in a test.
If the code encodes a value wrongly, a test that uses the same function encodes it wrongly too, and still passes.
`scripts/check.sh` fails when a test in `bttest` calls either function.

## Check behaviour against production

A parity case sends the same requests to a real Bigtable table, to the emulator built from this checkout, and to Google's emulator, and then compares the results.
`scripts/parity/run.sh` runs every parity case.
The cases in `scripts/parity/table-cases.mjs` test table admin calls, so each case creates its own tables in the real table's instance, named `better-bttest-parity-<run id>-t<n>`.
The script needs Docker and Google Cloud credentials, so CI does not run it.
The credentials must be able to write to the table, and to create and delete tables in its instance.
To set up the credentials, run `gcloud auth application-default login`.

To check how production behaves, follow these steps:

1. Create a table with these column families, each with the garbage collection rule `never`:
   - an int64 MIN aggregate family
   - an int64 MAX aggregate family
   - an int64 Sum aggregate family
   - a family with no value type
2. Add a case to `scripts/parity/cases.mjs`, or to `scripts/parity/table-cases.mjs` for a table admin call.
3. Run the script:

   ```sh
   PARITY_PROJECT=<project> PARITY_INSTANCE=<instance> PARITY_TABLE=<table> \
   PARITY_MIN_FAMILY=<MIN family> PARITY_MAX_FAMILY=<MAX family> PARITY_SUM_FAMILY=<Sum family> \
   PARITY_PLAIN_FAMILY=<plain family> \
   scripts/parity/run.sh
   ```

For each case where an emulator gives a different result from the real table, the script prints a diff.
Then it lists every case, and says for each emulator whether the case matched.
Before it exits, it deletes the rows it wrote and the tables it created, even after a failure or an interrupt.

The script exits with:

- 0 when every case matches on this checkout's emulator.
- 1 when a case differs on this checkout's emulator.
  A difference on Google's emulator shows in the report, and does not change the exit code.
- 2 when a `PARITY_*` variable is not set, or the script cannot use the login or the table.
- The failed step's code when a step such as `npm ci` or `docker build` fails.
- 130 on Ctrl-C or SIGTERM.

If the script cannot delete its rows or tables, it exits with 1, whatever the code would have been.

## Sync with upstream

The fork follows upstream's bigtable releases.
The `cloud.google.com/go/bigtable` version in `go.mod` is the release that the fork is on.

| Upstream path           | Fork path      |
|-------------------------|----------------|
| `bigtable/bttest`       | `bttest`       |
| `bigtable/cmd/emulator` | `cmd/emulator` |

The `upstream` branch holds Google's files unchanged, with one commit per release.
`main` merges the `upstream` branch, so git can tell the fork's changes from Google's when it merges a new release.
To merge the newest release, or a release you name, run:

```sh
scripts/sync-upstream.sh
scripts/sync-upstream.sh bigtable/v1.59.0
```

The comment at the top of the script says what the script changes and where it stops.
When the script finishes, push both branches with `git push origin main upstream`.
