# better-bttest

A fork of the Cloud Bigtable emulator (`bttest` and `cbtemulator`) from [googleapis/google-cloud-go](https://github.com/googleapis/google-cloud-go).
It adds the production behaviour that the upstream emulator lacks.

## Upstream

Forked from tag `bigtable/v1.58.0` (`967a532a6d1f3ffe5210c7f972b82b20ef663c2d`).

| Upstream path | Fork path |
| --- | --- |
| `bigtable/bttest` | `bttest` |
| `bigtable/cmd/emulator` | `cmd/emulator` |

The files keep upstream's names and layout, so an upstream diff applies directly.
From a google-cloud-go checkout, with `OLD` as the recorded tag and `NEW` as the target:

```sh
git diff OLD NEW -- bigtable/bttest bigtable/cmd/emulator | git -C ../better-bttest apply -p2 --3way
```

Then update the tag above and the `cloud.google.com/go/bigtable` version in `go.mod`.

The files under `bttest` and `cmd/emulator` are modified from upstream. The git history records each change.

## Run

```sh
go run ./cmd/emulator -host 0.0.0.0 -port 8086
```

Point a client at it with `BIGTABLE_EMULATOR_HOST=localhost:8086`.

## License

Apache License 2.0, as upstream. See `LICENSE`.
