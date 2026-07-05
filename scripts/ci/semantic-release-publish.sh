#!/usr/bin/env sh
set -eu

repo_dir="$(pwd)"
cd "${repo_dir}"

git config --global --add safe.directory "${repo_dir}" 2>/dev/null || true
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	echo "semantic-release must run from a Git checkout, but ${repo_dir} is not a Git repository." >&2
	exit 1
fi

exec npx --yes \
	--package semantic-release@25.0.5 \
	--package @semantic-release/commit-analyzer@13.0.1 \
	--package @semantic-release/release-notes-generator@14.1.1 \
	--package @semantic-release/github@12.0.9 \
	--package conventional-changelog-conventionalcommits@9.3.1 \
	semantic-release "$@"
