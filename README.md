# better-bttest

A fork of the Cloud Bigtable emulator (`bttest` and `cbtemulator`) from [googleapis/google-cloud-go](https://github.com/googleapis/google-cloud-go).
It adds the production behaviour that the upstream emulator lacks.

## Run

```sh
docker compose up --build -d
```

This builds the emulator and serves it on `localhost:8086`, in a container named `bigtable`.
Point a client at it with `BIGTABLE_EMULATOR_HOST=localhost:8086`, and stop it with `docker compose down`.

`go doc ./cmd/emulator` lists the emulator's flags, for a run without Docker.
`go doc ./bttest` shows how a Go test runs the emulator in its own process.
The repository is private, so `go get github.com/jayfeng0625/better-bttest` needs `GOPRIVATE=github.com/jayfeng0625/*` and git access to GitHub.

## Images

Each push to `main` publishes two images to GHCR, tagged with the full commit SHA:

- `ghcr.io/jayfeng0625/better-bttest`: the emulator.
- `ghcr.io/jayfeng0625/better-bttest-init`: the init image, with bash and [`cbt`](https://pkg.go.dev/cloud.google.com/go/cbt).

Each tag is one index holding linux/amd64 and linux/arm64, so one digest serves both platforms.
From a checkout, `docker build .` builds the emulator image, and `docker build --target init .` the init image.
The images are private while the repository is, so log in first with a token that has `read:packages`:

```sh
gh auth token | docker login ghcr.io -u <GitHub user> --password-stdin
```

Print a commit's index digest:

```sh
docker buildx imagetools inspect ghcr.io/jayfeng0625/better-bttest:<commit SHA> --format '{{.Manifest.Digest}}'
```

A compose file pins each image by digest.
The emulator image reports healthy once the emulator serves, so the init image can wait for it and run a table script:

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
Families take cbt's `name:gcrule[:intmin]` form; quote them, since `||` is a shell operator:

```bash
#!/usr/bin/env bash
set -euo pipefail
cbt -project demo -instance demo createtable events \
    'families=recent:maxage=1s||maxversions=1,lowest:never:intmin'
```

## Upstream

The fork tracks google-cloud-go's bigtable releases.
The `cloud.google.com/go/bigtable` version in `go.mod` is the release it is on.

| Upstream path           | Fork path      |
|-------------------------|----------------|
| `bigtable/bttest`       | `bttest`       |
| `bigtable/cmd/emulator` | `cmd/emulator` |

The `upstream` branch holds each upstream file unmodified, one commit per imported release, and `main` merges it, so git has the right base for a 3-way merge.
To merge the latest release, or a named one:

```sh
scripts/sync-upstream.sh
scripts/sync-upstream.sh bigtable/v1.59.0
```

The script's header comment says what it changes and where it stops.
Push both branches after it: `git push origin main upstream`.

The files under `bttest` and `cmd/emulator` are modified from upstream. The git history records each change.

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

Apache License 2.0, as upstream. See `LICENSE`.
