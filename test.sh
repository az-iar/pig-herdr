#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/extensions/pig-herdr"
sdk="${PIG_SDK_PATH:-$(pig reload --sdk-path)}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cp go.mod "$tmp/test.mod"
go mod edit -modfile="$tmp/test.mod" \
  -replace="github.com/MichaelKinsy/PiG/extensions/sdk=$sdk"
go test -mod=mod -modfile="$tmp/test.mod" -race ./...
go vet -mod=mod -modfile="$tmp/test.mod" ./...
