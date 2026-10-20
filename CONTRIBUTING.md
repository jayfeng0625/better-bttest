# better-bttest contribution guide

## Run the checks

```sh
scripts/check.sh
```

The script runs the same checks as CI: gofmt, the license headers, `go mod tidy`, the test value encoding, `go vet`, staticcheck, and the tests under the race detector.
It runs the last three twice: on the plain build, and with `-tags googlesql`, which builds the SQL planner in.
CI runs it on the latest Go 1.26 patch release, with `GOTOOLCHAIN=local`.

## Keep SQL behind the build tag

The SQL planner in `bttest/internal/sqlengine` imports go-googlesql, which makes the emulator binary about ten times larger, so it builds only with the `googlesql` tag.
Start a file that imports go-googlesql with `//go:build googlesql`, and a test that runs SQL or creates a view on the emulator too.
The plan types in `plan.go` and the code that runs a plan build either way.
Without the tag, `nosql.go` answers `Prepare` and a view read with `Unimplemented`, and its `CheckView` accepts any view query.

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
