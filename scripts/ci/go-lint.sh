#!/usr/bin/env sh
set -eu

timeout="${GOLANGCI_LINT_TIMEOUT:-5m}"

go vet ./...
golangci-lint run ./... --timeout="${timeout}"
