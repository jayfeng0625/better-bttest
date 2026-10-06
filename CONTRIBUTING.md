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

A parity case is a list of calls.
A run makes the calls on a real Bigtable table and on the emulator from this checkout, and compares each call's result.
The cases are in `internal/parity/cases.go`.
CI does not run them. Run them on a real instance when you change the emulator's behaviour or a case:

```sh
go test ./internal/parity -run 'TestParity$' -real=<project>/<instance>
```

The run uses Application Default Credentials. To set them up, run `gcloud auth application-default login`.
The credentials must be able to write rows, and to create and delete tables, in the instance.

The data cases write rows to the table `better-bttest-parity`.
Create it with the families that `families()` in `internal/parity/fixture.go` defines.
The run checks the table's families before it runs a case.
A case that creates a table names it `better-bttest-parity-<run id>-t<n>`.
When the test ends, it deletes the rows that its cases wrote and the tables that they created, and fails if any remain.
An interrupted run leaves its rows and tables, and a run that starts an hour or more later deletes them.

The view cases run only with `-views`:

```sh
go test ./internal/parity -run 'TestParity$' -real=<project>/<instance> -views -timeout 40m
```

They create materialized views named `better-bttest-parity-<run id>-v<n>` on their case tables, and production takes one to two minutes to create each.
The run deletes each view before its table.

### Compare Google's emulator

```sh
go test ./internal/parity -run TestStock -v -stock -real=<project>/<instance>
```

The run starts Google's stock emulator in Docker, runs the cases on it and on the real table, and logs each case whose results differ.
A difference does not fail the test.
A case that stops the emulator logs its panic, and the next case gets a fresh emulator.

### Add a parity case

A case is a `Case` in `internal/parity/cases.go`.
Each of its `Setup` calls must succeed, and the run compares the result of each of its `Calls`.
A read is a call too, so a case reads a row with `Read` or `ReadRow` where its results need the row's cells.
The comments on the call types in `case.go` say what each call sends.

Add the case to `MergeCases` when its results depend on how the family merges a write, so that it runs once for each aggregate family.
Add any other case on an aggregate family to `AggregateCases`, which runs once, on the Sum family.
Add the rest to `PlainCases` or `TableCases`.
Then run the cases on a real instance.
If a case needs another family, add it to `families()` in `fixture.go` and to the real table.

A SQL case's calls are `PrepareQuery` and `ExecuteQuery`, and `Query(sql, params...)` returns both.
`{table}` in the SQL stands for the case's table. Write it in backquotes, because GoogleSQL does not parse the table id unquoted.
A SQL case starts its `Setup` with `sqlFixture()`, which creates the table and writes the six rows that the queries read.
Add SQL cases to `SQLCases` in `internal/parity/sqlcases.go`.
A case for GROUP BY, UNNEST, or a function that `totalsQuery` calls goes in `TotalsCases` in `internal/parity/totalscases.go`.

A view case creates its view with `CreateView`, and `{view}` in its SQL stands for the view.
`viewFixture(query)` writes the SQL fixture, then creates the view, so that the view's first refresh holds every row.
Give a view case `Deadline: viewDeadline`, and add it to `ViewCases` in `internal/parity/viewcases.go`.

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
