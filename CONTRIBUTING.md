# better-bttest contribution guide

## Run the checks

```sh
scripts/check.sh
```

The script runs the checks that CI runs: gofmt, the license headers, `go mod tidy`, `go vet`, staticcheck, and the tests under the race detector.
CI runs it on the newest Go 1.26 patch, with `GOTOOLCHAIN=local`.

## Change an upstream file

An upstream file is a file that `scripts/sync-upstream.sh` imports from google-cloud-go, such as `bttest/inmem.go`.
Keep each upstream file close to upstream's, so that [the upstream sync](README.md#upstream) merges cleanly:

- Put new code in new files. Change an upstream file only where the new code hooks in.
- To replace an upstream function, delete it from the upstream file.
  Define it under the same name in a new file, as `bttest/gc.go` does for `applyGC`.
  A later upstream edit to the function then stops the merge with a conflict.
- Keep upstream's style, comments, and log lines verbatim. No test checks the log lines.

## Add the license header

Start a new Go file with `// SPDX-License-Identifier: Apache-2.0`.
Keep Google's Apache-2.0 header in an upstream file.

## Test the stored bytes

Write each test input and expected value with `binary.BigEndian`, in the bytes that production stores.
Upstream's aggregate tests in `bttest/inmem_test.go` do the same.
A test that uses the code's own codec, such as `encodeInt64`, still passes when the code and the test share an encoding bug.

## Check behaviour against production

`scripts/parity/run.sh` runs each parity case against a real Bigtable table, the emulator image built from this checkout, and Google's stock emulator.
The script needs Docker, and Application Default Credentials that can write to the table, so CI does not run it.

To check a claim about production's behaviour:

1. Create a table with these families, each with GC rule `never`:
   - an int64 MIN aggregate family
   - an int64 MAX aggregate family
   - an int64 Sum aggregate family
   - a family with no value type
2. Add a case to `scripts/parity/cases.mjs`.
3. Run the script:

   ```sh
   PARITY_PROJECT=<project> PARITY_INSTANCE=<instance> PARITY_TABLE=<table> \
   PARITY_MIN_FAMILY=<MIN family> PARITY_MAX_FAMILY=<MAX family> PARITY_SUM_FAMILY=<Sum family> \
   PARITY_PLAIN_FAMILY=<plain family> \
   scripts/parity/run.sh
   ```

The script prints a diff for each case where an emulator differs from the real table.
Then it lists every case, with a match or a difference for each emulator.
Before it exits, it deletes the rows it wrote, even after a failure or an interrupt.
Its exit codes mean:

- 0 when every case matches on this checkout's emulator.
- 1 when a case differs on this checkout's emulator. A stock emulator difference shows only in the report.
- 2 when a `PARITY_*` variable is unset, or the login or the table is not usable.
