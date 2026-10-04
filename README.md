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

To run the emulator without Docker, see the [`emulator` command docs](https://pkg.go.dev/github.com/jayfeng0625/better-bttest/cmd/emulator).
A Go test can also run the emulator in the test's own process, as the [`bttest` package docs](https://pkg.go.dev/github.com/jayfeng0625/better-bttest/bttest) show.
Add the module with `go get github.com/jayfeng0625/better-bttest`.

## Images

Each push to `main` publishes two images to GHCR, tagged with the full commit SHA:

- `ghcr.io/jayfeng0625/better-bttest`: the emulator.
- `ghcr.io/jayfeng0625/better-bttest-init`: the init image, with bash and [`cbt`](https://pkg.go.dev/cloud.google.com/go/cbt).

Each tag is one image index with a linux/amd64 and a linux/arm64 image, so one digest works on both platforms.
From a checkout, `docker build .` builds the emulator image, and `docker build --target init .` builds the init image.

Print a commit's index digest:

```sh
docker buildx imagetools inspect ghcr.io/jayfeng0625/better-bttest:<commit SHA> --format '{{.Manifest.Digest}}'
```

The compose file below pins each image by digest.
The emulator image has a healthcheck that passes once the emulator serves.
`condition: service_healthy` makes the init container wait for that healthcheck, and then the init container runs a table script:

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
Where that section does not state the behaviour, a [parity case](CONTRIBUTING.md#check-behaviour-against-production) shows it on a real table.

- **[Intersection GC rules](https://cloud.google.com/bigtable/docs/garbage-collection#combinations).**
  GC removes a cell only when every rule in the intersection would remove it.
  The stock emulator logs that it does not support the rule, and keeps every cell.
- **[MIN and MAX aggregates](https://cloud.google.com/bigtable/docs/writes#increments).**
  A MIN family merges its inputs into their minimum, and a MAX family merges them into their maximum.
  The stock emulator merges only Sum, so a MIN or MAX cell keeps the last input.
- **[Family types](https://cloud.google.com/bigtable/docs/data-types#aggregates).**
  A write that does not fit its family's type fails with production's error.
  The stock emulator accepts a `SetCell`, an increment, or an append on an aggregate family.
- **[Aggregate inputs](https://cloud.google.com/bigtable/docs/reference/data/rpc/google.bigtable.v2#mergetocell).**
  A `MergeToCell` input is a `bytes_value` that holds an int64 as [8 big-endian bytes](https://cloud.google.com/bigtable/docs/data-types#aggregates).
  A missing input is [NULL](https://cloud.google.com/bigtable/docs/reference/data/rpc/google.bigtable.v2#value), so an `AddToCell` adds 0 and a `MergeToCell` changes nothing.
  The stock emulator takes a `MergeToCell` input only as a `raw_value`, and crashes on a missing input.
- **[SQL queries](https://cloud.google.com/bigtable/docs/googlesql-overview).**
  `PrepareQuery` and `ExecuteQuery` run a `SELECT` over one table. [GoogleSQL's analyzer](https://github.com/goccy/go-googlesql) types each query.
  A query can use a select list with aliases and `*`, a subquery in `FROM`, `WHERE`, `ORDER BY`, `LIMIT`, `fam['col']`, comparisons, `AND`, `OR`, `NOT`, `IS NULL`, `IN`, `BETWEEN`, `LIKE`, `STARTS_WITH`, `CAST` between `BYTES` and `STRING`, `TO_INT64`, and query parameters.
  Any other construct fails `PrepareQuery` with `InvalidArgument`.
  A prepared query expires 40 s after `PrepareQuery`, or once a family it reads is dropped, with production's error.
  The stock emulator returns `Unimplemented`.

Two more changes have no production counterpart:

- The image's healthcheck runs `emulator -probe`, which the [`emulator` command docs](https://pkg.go.dev/github.com/jayfeng0625/better-bttest/cmd/emulator) describe.
- On SIGTERM, the emulator prints its shutdown line, closes the server, and exits, as it does on an interrupt.

## Not supported yet

The fork does not support these yet:

- SQL beyond the constructs listed above, such as `GROUP BY`, `UNNEST`, and `OFFSET`.
- A runtime SQL error's second line, `(while evaluating <expression>)`.
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

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) gives the checks to run and the rules for a change.

## License

The fork uses the Apache License 2.0, as upstream does. See `LICENSE`.
