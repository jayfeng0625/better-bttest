#!/usr/bin/env bash
# Run the checks that CI runs: formatting, license headers, module tidiness,
# vet, staticcheck, and the tests under the race detector.
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

# go mod tidy -diff exits 1 on a module download failure too, and then it
# prints no diff.
if ! tidy_diff=$(go mod tidy -diff); then
    if [ -n "$tidy_diff" ]; then
        echo "$tidy_diff"
        echo "Run go mod tidy." >&2
    fi
    exit 1
fi

go vet ./...

# staticcheck exits 1 on any finding, so the filtered output decides. The
# filtered findings sit in upstream code, which keeps upstream's style.
staticcheck_bin=$(mktemp -d)
trap 'rm -rf "$staticcheck_bin"' EXIT
GOBIN=$staticcheck_bin go install honnef.co/go/tools/cmd/staticcheck@v0.8.1
findings=$("$staticcheck_bin/staticcheck" ./... 2>&1 |
    grep -v -e '^bttest/example_test.go:[0-9:]* google.golang.org/grpc.Dial is deprecated' \
        -e '^bttest/inmem.go:[0-9:]* const maxValidMilliSeconds is unused' \
        -e '^cmd/emulator/cbtemulator.go:[0-9:]* should use a simple channel send/receive' || true)
if [ -n "$findings" ]; then
    echo "$findings" >&2
    exit 1
fi

go test -race ./...
