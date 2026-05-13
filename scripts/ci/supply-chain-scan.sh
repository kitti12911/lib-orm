#!/usr/bin/env sh
set -eu

scan_ref="${TRIVY_SCAN_REF:-.}"
severity="${TRIVY_SEVERITY:-CRITICAL,HIGH}"
scanners="${TRIVY_SCANNERS:-vuln,secret,misconfig}"
ignore_unfixed="${TRIVY_IGNORE_UNFIXED:-true}"
gitleaks_source="${GITLEAKS_SOURCE:-.}"

set -- trivy fs \
	--scanners "${scanners}" \
	--exit-code 1 \
	--severity "${severity}"

if [ "${ignore_unfixed}" = "true" ]; then
	set -- "$@" --ignore-unfixed
fi

set -- "$@" "${scan_ref}"

"$@"
gitleaks detect --source "${gitleaks_source}" --redact --exit-code 1
