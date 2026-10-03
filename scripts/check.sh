#!/usr/bin/env bash
# Run the checks that CI runs: formatting, license headers, module tidiness,
# vet, and the tests under the race detector.
# Usage: scripts/check.sh
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
    echo "Run gofmt -w on:" >&2
    echo "$unformatted" >&2
    exit 1
fi

# Upstream files keep Google's Apache-2.0 header. The fork's own files start
# with an SPDX line.
unlicensed=$(git ls-files --cached --others --exclude-standard '*.go' |
    xargs grep -L -e 'Licensed under the Apache License, Version 2.0' \
        -e '^// SPDX-License-Identifier: Apache-2.0$' || true)
if [ -n "$unlicensed" ]; then
    echo "Start each of these files with // SPDX-License-Identifier: Apache-2.0" >&2
    echo "$unlicensed" >&2
    exit 1
fi

if ! go mod tidy -diff; then
    echo "Run go mod tidy." >&2
    exit 1
fi

go vet ./...
go test -race ./...
