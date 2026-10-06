# better-bttest

better-bttest is a Bigtable emulator for local development and tests.
It is a fork of Google's emulator, which is the `bttest` package and the `cbtemulator` command in [googleapis/google-cloud-go](https://github.com/googleapis/google-cloud-go).
The fork acts like production Bigtable in the places where Google's emulator does not.
[What the fork changes](#what-the-fork-changes) lists them.

## Start the emulator

```sh
docker compose up --build -d
```

The command builds the emulator and starts it on `localhost:8086`, in a container named `bigtable`.
To connect a Bigtable client, set `BIGTABLE_EMULATOR_HOST=localhost:8086`.
To stop the emulator, run `docker compose down`.

You can also run the emulator in two other ways:

- Without Docker, as the [`emulator` command docs](https://pkg.go.dev/github.com/jayfeng0625/better-bttest/cmd/emulator) show.
- Inside a Go test's own process, as the [`bttest` package docs](https://pkg.go.dev/github.com/jayfeng0625/better-bttest/bttest) show.
  Add the module with `go get github.com/jayfeng0625/better-bttest`.

## What the fork changes

Each entry links to the Bigtable docs for the feature.
Where the docs do not say how production behaves, a parity case shows it.
A parity case sends the same requests to a real table and to the emulator.
[Check behaviour against production](CONTRIBUTING.md#check-behaviour-against-production) says how to run the cases.

- **[Intersection garbage collection rules](https://cloud.google.com/bigtable/docs/garbage-collection#combinations).**
  An intersection joins rules with AND.
  The emulator deletes a cell only when every rule in the intersection would delete it.
  Google's emulator logs that it does not support the rule, and keeps every cell.
- **[MIN and MAX aggregate families](https://cloud.google.com/bigtable/docs/writes#increments).**
  A MIN family keeps the smallest value written to a cell, and a MAX family keeps the largest.
  Google's emulator combines values only in a Sum family, so a MIN or MAX cell keeps the last value written.
- **[Writes that do not match the family type](https://cloud.google.com/bigtable/docs/data-types#aggregates).**
  A `SetCell`, an increment, or an append to an aggregate family fails with the error that production returns.
  Google's emulator accepts these writes.
- **[`MergeToCell` values](https://cloud.google.com/bigtable/docs/reference/data/rpc/google.bigtable.v2#mergetocell).**
  `MergeToCell` takes its value in `bytes_value`, as an int64 in [8 big-endian bytes](https://cloud.google.com/bigtable/docs/data-types#aggregates).
  Google's emulator reads the value only from `raw_value`.
- **[Missing values](https://cloud.google.com/bigtable/docs/reference/data/rpc/google.bigtable.v2#value).**
  A request with no value counts as NULL, so an `AddToCell` adds 0 and a `MergeToCell` changes nothing.
  Google's emulator crashes on a missing value.
- **[SQL queries](https://cloud.google.com/bigtable/docs/googlesql-overview).**
  `PrepareQuery` and `ExecuteQuery` run a `SELECT` on one table. The analyzer in [go-googlesql](https://github.com/goccy/go-googlesql) checks the types in each query.
  A prepared query expires 40 s after `PrepareQuery`, or once a family it reads is dropped.
  `ExecuteQuery` then fails as production does, with `FailedPrecondition` and a `PREPARED_QUERY_EXPIRED` violation.
  Google's emulator returns `Unimplemented`.
  A query parameter takes the type `BYTES`, `STRING`, or `INT64`.
  Any other type fails `PrepareQuery` with `InvalidArgument`.
  A construct outside the list below fails `PrepareQuery` with `InvalidArgument`.
  A query can take query parameters and use these constructs:
  - Query parts: a select list with aliases and `*`, a subquery in `FROM`, a comma join with `UNNEST`, `WHERE`, `GROUP BY`, `HAVING`, `ORDER BY`, and `LIMIT`.
  - Operators: comparisons, `AND`, `OR`, `NOT`, `IS NULL`, `IN`, `BETWEEN`, `LIKE`, `DIV`, `-` on `INT64`, searched `CASE`, the map subscript `fam['col']`, and the array subscripts `[n]` and `[OFFSET(n)]`.
  - Functions: `STARTS_WITH`, `CAST` between `BYTES` and `STRING`, `TO_INT64`, `COALESCE`, `SPLIT` on `BYTES`, `JSON_QUERY_ARRAY` with the path `$`, and `ARRAY_CONCAT`.
  - Aggregate functions: `COUNT(*)`, `SUM` over `INT64`, and `MAX`.
  - Values: array literals.

Two more changes have nothing to match in production:

- The image has a Docker healthcheck.
  It runs `emulator -probe`, which the [`emulator` command docs](https://pkg.go.dev/github.com/jayfeng0625/better-bttest/cmd/emulator) describe.
- On SIGTERM, such as from `docker stop`, the emulator prints its shutdown message, closes the server, and exits.
  It does the same on Ctrl-C.

## Not supported yet

- SQL beyond the constructs listed above, such as `LEFT JOIN`, `COUNT(expr)`, and `LIMIT` with `OFFSET`.
- A runtime SQL error's second line, `(while evaluating <expression>)`.
- Materialized views.
- HyperLogLog (HLL) aggregate families.

## Use the published images

Each push to `main` publishes two images to the GitHub Container Registry, tagged with the full commit SHA:

- `ghcr.io/jayfeng0625/better-bttest` is the emulator.
- `ghcr.io/jayfeng0625/better-bttest-init` has bash and [`cbt`](https://pkg.go.dev/cloud.google.com/go/cbt), to create tables when the emulator starts.

Each tag runs on linux/amd64 and linux/arm64, and has one digest for both.
To build the images from a checkout, run `docker build .` for the emulator and `docker build --target init .` for the init image.

### Pin an image by digest

A digest names one exact build of an image.
To print the digest for a commit, run:

```sh
docker buildx imagetools inspect ghcr.io/jayfeng0625/better-bttest:<commit SHA> --format '{{.Manifest.Digest}}'
```

### Check how an image was built

Each publish adds a signed build record to both images, with the [`actions/attest` action](https://github.com/actions/attest#readme).
The record names the repository, the workflow, and the commit that built the image.
To check it, run [`gh attestation verify`](https://cli.github.com/manual/gh_attestation_verify):

```sh
gh attestation verify oci://ghcr.io/jayfeng0625/better-bttest@sha256:<digest> --owner jayfeng0625
```

### Create tables when the emulator starts

This compose file starts the emulator, and then runs your table script in the init image.
The init container waits until the emulator's healthcheck passes:

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

`cbt` finds the emulator through `BIGTABLE_EMULATOR_HOST`, and needs no credentials:

```bash
#!/usr/bin/env bash
set -euo pipefail
cbt -project demo -instance demo createtable events \
    'families=recent:maxage=1s||maxversions=1,lowest:never:intmin'
```

Each column family in the list takes the form `name:gcrule[:type]`.
The optional type makes an aggregate family: `intsum`, `intmin`, or `intmax`.
In the example, `recent` deletes a cell once the cell is older than 1 second or is not the newest version.
`lowest` is a MIN family that never deletes a cell.
Quote the list, because bash reads `||` as an operator.

## Contributing

[`CONTRIBUTING.md`](CONTRIBUTING.md) lists the checks to run and the rules for a change.
It also explains how the fork takes in each new google-cloud-go bigtable release.
The `cloud.google.com/go/bigtable` version in `go.mod` is the release that the fork is on.

## License

The fork uses the Apache License 2.0, as google-cloud-go does. See `LICENSE`.
