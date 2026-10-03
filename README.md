# better-bttest

better-bttest is a fork of the Cloud Bigtable emulator (`bttest` and `cbtemulator`) from [googleapis/google-cloud-go](https://github.com/googleapis/google-cloud-go).
It behaves like production Bigtable where the stock emulator does not, as [Differences from upstream](#differences-from-upstream) lists.

## Run

```sh
docker compose up --build -d
```

The command builds the emulator and serves it on `localhost:8086`, in a container named `bigtable`.
Point a client at it with `BIGTABLE_EMULATOR_HOST=localhost:8086`.
Stop it with `docker compose down`.

To run the emulator without Docker, see `go doc ./cmd/emulator`.
`go doc ./bttest` shows how a Go test runs the emulator in its own process.
The repository is private, so `go get github.com/jayfeng0625/better-bttest` needs `GOPRIVATE=github.com/jayfeng0625/*` and git access to GitHub.

## Images

Each push to `main` publishes two images to GHCR, tagged with the full commit SHA:

- `ghcr.io/jayfeng0625/better-bttest`: the emulator.
- `ghcr.io/jayfeng0625/better-bttest-init`: the init image, with bash and [`cbt`](https://pkg.go.dev/cloud.google.com/go/cbt).

Each tag is one image index with a linux/amd64 and a linux/arm64 image, so one digest works on both platforms.
From a checkout, `docker build .` builds the emulator image, and `docker build --target init .` builds the init image.
The images are private while the repository is, so log in first with a token that has `read:packages`:

```sh
gh auth token | docker login ghcr.io -u <GitHub user> --password-stdin
```

Print a commit's index digest:

```sh
docker buildx imagetools inspect ghcr.io/jayfeng0625/better-bttest:<commit SHA> --format '{{.Manifest.Digest}}'
```

The compose file below pins each image by digest.
The emulator image reports healthy once the emulator serves, so the init image waits for it and then runs a table script:

```yaml
services:
  bigtable:
    image: ghcr.io/jayfeng0625/better-bttest@sha256:<digest>
  bigtable-init:
    image: ghcr.io/jayfeng0625/better-bttest-init@sha256:<digest>
    depends_on:
      bigtable:
        condition: service_healthy
    environment:
      BIGTABLE_EMULATOR_HOST: bigtable:8086
    volumes:
      - ./create-tables.sh:/create-tables.sh:ro
    command: ["bash", "/create-tables.sh"]
```

`cbt` reads `BIGTABLE_EMULATOR_HOST` and needs no credentials.
Each family takes cbt's `name:gcrule[:intmin]` form.
Quote the families, because `||` is a shell operator:

```bash
#!/usr/bin/env bash
set -euo pipefail
cbt -project demo -instance demo createtable events \
    'families=recent:maxage=1s||maxversions=1,lowest:never:intmin'
```

## Differences from upstream

The fork supports these production Bigtable features, which the stock emulator does not.
Each entry links the nearest section of the Cloud Bigtable documentation.
Where that section does not state the behaviour, a [parity case](#parity-with-production) shows it on a real table.

- **[Intersection GC rules](https://cloud.google.com/bigtable/docs/garbage-collection#combinations).**
  GC removes a cell only when every rule in the intersection would remove it.
  The stock emulator logs that it does not support the rule, and keeps every cell.
- **[MIN and MAX aggregates](https://cloud.google.com/bigtable/docs/writes#increments).**
  A MIN family merges its inputs into their minimum, and a MAX family merges them into their maximum.
  The stock emulator merges only Sum, so a MIN or MAX cell keeps the last input.
- **[Family types](https://cloud.google.com/bigtable/docs/data-types#aggregates).**
  A write that does not fit its family's type fails with production's error.
  The stock emulator accepts a `SetCell` or a `ReadModifyWriteRow` on an aggregate family.
- **[Aggregate inputs](https://cloud.google.com/bigtable/docs/reference/data/rpc/google.bigtable.v2#mergetocell).**
  A `MergeToCell` input is a `bytes_value` that holds an int64 as 8 big-endian bytes.
  A missing input is NULL, so an `AddToCell` adds 0 and a `MergeToCell` changes nothing.
  The stock emulator takes a `MergeToCell` input only as a `raw_value`, and crashes on a missing input.

Two more changes have no production counterpart:

- The image's healthcheck runs `emulator -probe`, which `go doc ./cmd/emulator` describes.
- The emulator shuts down cleanly on SIGTERM, as it does on an interrupt.

## Not supported yet

The fork does not support these yet:

- SQL queries. `PrepareQuery` and `ExecuteQuery` return `Unimplemented`.
- Materialized views.
- HyperLogLog (HLL) aggregate families.

## Upstream

The fork tracks google-cloud-go's bigtable releases.
The `cloud.google.com/go/bigtable` version in `go.mod` is the release it is on.

| Upstream path           | Fork path      |
|-------------------------|----------------|
| `bigtable/bttest`       | `bttest`       |
| `bigtable/cmd/emulator` | `cmd/emulator` |

The `upstream` branch holds each upstream file unmodified, with one commit per imported release.
`main` merges the `upstream` branch, so git has the right base for a 3-way merge.
To merge the latest release, or a named one:

```sh
scripts/sync-upstream.sh
scripts/sync-upstream.sh bigtable/v1.59.0
```

The script's header comment says what it changes and where it stops.
After the script finishes, push both branches with `git push origin main upstream`.

## Parity with production

`scripts/parity/run.sh` runs each parity case against a real Bigtable table, the emulator image built from this checkout, and Google's stock emulator.
It prints a diff for each case where an emulator differs from the real table, then a table of every case.
It needs Docker, and Application Default Credentials that can write to the table, so CI does not run it.
The table needs an int64 MIN aggregate family, an int64 MAX aggregate family, and a family with no value type, each with GC rule `never`:

```sh
PARITY_PROJECT=<project> PARITY_INSTANCE=<instance> PARITY_TABLE=<table> \
PARITY_MIN_FAMILY=<MIN family> PARITY_MAX_FAMILY=<MAX family> PARITY_PLAIN_FAMILY=<plain family> \
scripts/parity/run.sh
```

It exits 0 when every case matches on this checkout's emulator, 1 on a difference there, and 2 when the login or the table is not usable.
A stock emulator difference only shows in the report.
It deletes the rows it wrote, even after a failure or an interrupt.

## License

The fork uses the Apache License 2.0, as upstream does. See `LICENSE`.
