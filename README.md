# better-bttest

A fork of the Cloud Bigtable emulator (`bttest` and `cbtemulator`) from [googleapis/google-cloud-go](https://github.com/googleapis/google-cloud-go).
It adds the production behaviour that the upstream emulator lacks.

## Run

```sh
docker compose up --build -d
```

This builds the emulator and serves it on `localhost:8086`, in a container named `bigtable`.
Point a client at it with `BIGTABLE_EMULATOR_HOST=localhost:8086`.
`docker compose down` stops it.

firecrest's emulator has the same name and port, so stop it first.
From firecrest's root, run `docker compose -f gcp/bigtable-gateway/scripts/docker-compose.yml down`.

Without Docker, run `go run ./cmd/emulator -host 0.0.0.0 -port 8086`.

### Run firecrest's tests against it

firecrest's `start-local-bigtable.sh` finds the running `bigtable` container and uses it.
It then skips table creation, so create the tables once for each new container.
From firecrest's root:

1. Create the tables: `gcp/bigtable-gateway/scripts/create-bigtable-tables.sh`.
   It needs `cbt` on the host (`gcloud components install cbt`).
2. Run the tests as usual: `pnpm run test` in `gcp/bigtable-gateway`, and `pnpm run test:bigtable` in `webscale/webscale-event-worker`.

### Use it in Go tests

A Go test can run the emulator in its own process, with no Docker.
Call `bttest.NewServer("localhost:0")` and dial its `Addr` with the Go client, as `bttest/example_test.go` does.
Each server holds its own tables, so tests that start their own server share no state.

The repository is private, so `go get github.com/jayfeng0625/better-bttest` needs `GOPRIVATE=github.com/jayfeng0625/*` and git access to GitHub.

## Upstream

The fork tracks google-cloud-go's bigtable releases.
The `cloud.google.com/go/bigtable` version in `go.mod` is the release it is on.

| Upstream path           | Fork path      |
|-------------------------|----------------|
| `bigtable/bttest`       | `bttest`       |
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

## License

Apache License 2.0, as upstream. See `LICENSE`.
