#!/usr/bin/env sh
set -eu

govulncheck ./...
semgrep scan --config=p/golang --config=p/secrets --error
