# better-bttest

A fork of the Cloud Bigtable emulator (`bttest` and `cbtemulator`) from [googleapis/google-cloud-go](https://github.com/googleapis/google-cloud-go).
It adds the production behaviour that the upstream emulator lacks.

## Upstream

The fork tracks google-cloud-go's bigtable releases.
The `cloud.google.com/go/bigtable` version in `go.mod` is the release it is on.

| Upstream path | Fork path |
| --- | --- |
| `bigtable/bttest` | `bttest` |
| `bigtable/cmd/emulator` | `cmd/emulator` |

The `upstream` branch holds upstream's files unmodified, one commit per imported release.
`main` merges it, so git has the right base for a 3-way merge.

To move to the latest release, or to a named one:

```sh
scripts/sync-upstream.sh
scripts/sync-upstream.sh bigtable/v1.59.0
```

The script imports the release onto `upstream` and merges it into a sync branch.
It moves `go.mod` to the same release, then builds and tests.
On success it fast-forwards `main`; on a conflict or a failure it stops on the sync branch.
It pushes nothing: run `git push origin main upstream` after it.

The files under `bttest` and `cmd/emulator` are modified from upstream. The git history records each change.

## Run

```sh
go run ./cmd/emulator -host 0.0.0.0 -port 8086
```

Point a client at it with `BIGTABLE_EMULATOR_HOST=localhost:8086`.

## License

Apache License 2.0, as upstream. See `LICENSE`.
