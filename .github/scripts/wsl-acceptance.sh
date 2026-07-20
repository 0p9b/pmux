#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${WSL_DISTRO_NAME:-}" ]] && ! grep -Eiq '(microsoft|wsl)' /proc/version; then
	echo "release-gated WSL job is not running inside WSL" >&2
	exit 1
fi
printf 'WSL distribution: %s\n' "${WSL_DISTRO_NAME:-detected-from-kernel}"

GOVER="$(grep '^go ' go.mod | awk '{print $2}')"
if ! command -v go >/dev/null 2>&1; then
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL "https://go.dev/dl/go${GOVER}.linux-amd64.tar.gz" -o "$tmp/go.tar.gz"
	sudo tar -C /usr/local -xzf "$tmp/go.tar.gz"
	export PATH="/usr/local/go/bin:${PATH}"
fi
printf 'using %s\n' "$(go env GOVERSION)"

with_tag="$(mktemp)"
without_tag="$(mktemp)"
added="$(mktemp)"
trap 'rm -f "$with_tag" "$without_tag" "$added"' EXIT

go list -tags=wsl_e2e \
	-f '{{range .TestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}' \
	./... | sed '/^$/d' | sort -u >"$with_tag"
go list \
	-f '{{range .TestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}' \
	./... | sed '/^$/d' | sort -u >"$without_tag"
comm -23 "$with_tag" "$without_tag" >"$added"
if [[ ! -s "$added" ]]; then
	echo "no wsl_e2e-tagged test is selected on the WSL runner" >&2
	exit 1
fi
echo "selected wsl_e2e test files:"
cat "$added"

export PMUX_RELEASE_E2E=1
go test -tags=wsl_e2e ./...
go run ./cmd/pmux version
