# better-bttest contribution guide

## Run the checks

```sh
scripts/check.sh
```

The script runs the checks that CI runs: gofmt, the license headers, `go mod tidy`, `go vet`, staticcheck, and the tests under the race detector.
CI runs it on the newest Go 1.26 patch, with `GOTOOLCHAIN=local`.

## Change an upstream file

An upstream file is a file that `scripts/sync-upstream.sh` imports from google-cloud-go, such as `bttest/inmem.go`.
The script merges each new release with `git merge`, so keep each upstream file close to upstream's:

- Put new code in new files. Change an upstream file only where the new code hooks in.
- To replace an upstream function, delete it from the upstream file and define it under the same name in a new file, as `bttest/gc.go` does for `applyGC`.
  A later upstream edit to the function then stops the merge with a conflict.
- Keep upstream's style, comments, and log lines verbatim. No test checks the log lines.
- Keep Google's Apache-2.0 header in an upstream file. Start a new Go file with `// SPDX-License-Identifier: Apache-2.0`.

## Test the stored bytes

Write a test's inputs and check its results in the bytes that production stores, with `encoding/binary`, as upstream's aggregate tests in `bttest/inmem_test.go` do.
Production stores an int64 aggregate as 8 big-endian bytes.
A test that encodes with the code's own codec, such as `encodeInt64`, still passes when the code and the test share an encoding bug.

## Check behaviour against production

`scripts/parity/run.sh` runs each parity case against a real Bigtable table, the emulator image built from this checkout, and Google's stock emulator.
It prints a diff for each case where an emulator differs from the real table, then a table of every case.
To check a claim about production's behaviour, add a case to `scripts/parity/cases.mjs` and run the script.
The script needs Docker, and Application Default Credentials that can write to the table, so CI does not run it.
The table needs these families, each with GC rule `never`:

- an int64 MIN aggregate family
- an int64 MAX aggregate family
- an int64 Sum aggregate family
- a family with no value type

`cbt createtable` creates such a table:

```sh
cbt -project <project> -instance <instance> createtable <table> \
    'families=min:never:intmin,max:never:intmax,sum:never:intsum,plain:never'
```

Run the cases against it:

```sh
PARITY_PROJECT=<project> PARITY_INSTANCE=<instance> PARITY_TABLE=<table> \
PARITY_MIN_FAMILY=<MIN family> PARITY_MAX_FAMILY=<MAX family> PARITY_SUM_FAMILY=<Sum family> \
PARITY_PLAIN_FAMILY=<plain family> \
scripts/parity/run.sh
```

It exits 0 when every case matches on this checkout's emulator, 1 on a difference there, and 2 when the login or the table is not usable.
A stock emulator difference only shows in the report.
It deletes the rows it wrote, even after a failure or an interrupt.
