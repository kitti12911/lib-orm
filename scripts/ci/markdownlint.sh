#!/usr/bin/env sh
set -eu

if command -v markdownlint-cli2 >/dev/null 2>&1; then
	exec markdownlint-cli2 "$@"
fi

exec env NPM_CONFIG_UPDATE_NOTIFIER=false NPM_CONFIG_FUND=false npx --yes markdownlint-cli2@0.22.1 "$@"
