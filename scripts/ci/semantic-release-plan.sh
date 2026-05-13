#!/usr/bin/env sh
set -eu

repo_dir="${CI_PROJECT_DIR:-$(pwd)}"
cd "${repo_dir}"

git config --global --add safe.directory "${repo_dir}" 2>/dev/null || true
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	echo "semantic-release must run from a Git checkout, but ${repo_dir} is not a Git repository." >&2
	exit 1
fi

log_file="${SEMANTIC_RELEASE_LOG:-${RUNNER_TEMP:-/tmp}/semantic-release-dry-run.log}"

set +e
semantic-release --dry-run >"${log_file}" 2>&1
status="$?"
set -e

cat "${log_file}"

if [ "${status}" -ne 0 ]; then
	exit "${status}"
fi

version="$(sed -nE \
	-e 's/.*next release version is ([0-9A-Za-z.+-]+).*/\1/p' \
	-e 's/.*The next release version is ([0-9A-Za-z.+-]+).*/\1/p' \
	"${log_file}" | tail -n 1)"

if [ -z "${version}" ]; then
	release_created=false
	tag_name=
else
	release_created=true
	tag_name="v${version}"
fi

echo "release_created=${release_created}"
if [ -n "${tag_name}" ]; then
	echo "tag_name=${tag_name}"
fi

if [ -n "${CI_OUTPUT_FILE:-}" ]; then
	{
		echo "release_created=${release_created}"
		echo "tag_name=${tag_name}"
	} >>"${CI_OUTPUT_FILE}"
fi
