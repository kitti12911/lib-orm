#!/usr/bin/env sh
set -eu

coverage_profile="${GO_COVERAGE_PROFILE:-coverage.out}"
race="${GO_TEST_RACE:-true}"
cgo="${GO_TEST_CGO:-${race}}"

if [ "${cgo}" = "true" ] && ! command -v gcc >/dev/null 2>&1; then
	echo "CGO-enabled tests require a C compiler in the toolchain image." >&2
	exit 2
fi

set -- go test -coverprofile="${coverage_profile}" -covermode=atomic

if [ "${race}" = "true" ]; then
	set -- "$@" -race
fi

set -- "$@" ./...

if [ "${cgo}" = "true" ]; then
	cgo_enabled=1
else
	cgo_enabled=0
fi

env CGO_ENABLED="${cgo_enabled}" "$@"
go tool cover -func="${coverage_profile}" | tail -n 1
