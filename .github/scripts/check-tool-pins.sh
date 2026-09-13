#!/usr/bin/env bash
#
# Check that each pinned tool version agrees in every place it is written down.
#
# Dependabot only looks at action SHAs and go.mod; it does not follow a version input handed
# to an action, a go run ...@vX, or the ShellCheck release we install ourselves. Those can
# only be bumped by hand, and anything bumped by hand eventually gets missed somewhere.
# Issue #73 was exactly that: CI was running latest while the docs said v2.6.2. The
# assumption that "local runs and CI run the same checks" breaks silently, so fail here.
set -euo pipefail

cd "$(dirname "$0")/../.."

status=0

# pin <label> <value>... : error unless every value is the same. An empty string means "could
# not be extracted", so that is a failure too (changing the spelling must not let the check
# pass silently).
pin() {
  local label=$1 first=$2 value
  shift
  for value in "$@"; do
    if [ -z "$value" ] || [ "$value" != "$first" ]; then
      printf '::error::%s: pinned versions disagree (or could not be read): %s\n' "$label" "$*"
      status=1
      return
    fi
  done
  printf 'ok  %-14s %s\n' "$label" "$first"
}

# version_in <file> <grep -E pattern> : extract x.y.z from the first matching line.
version_in() {
  grep -hoE "$2" "$1" | head -n 1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' || true
}

# golangci-lint: CI (the action's version input) and the go run in README / CLAUDE.md.
pin golangci-lint \
  "$(grep -A8 'golangci-lint-action@' .github/workflows/verify.yml |
    grep -hoE 'version: v?[0-9]+\.[0-9]+\.[0-9]+' | head -n 1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in README.md 'golangci-lint@v[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in CLAUDE.md 'golangci-lint@v[0-9]+\.[0-9]+\.[0-9]+')"

# GoReleaser: unless the PR cross-build (verify.yml) and the actual release (release.yml) use
# the same version, the config a PR validated and the GoReleaser that builds the release differ.
pin goreleaser \
  "$(grep -A8 'goreleaser-action@' .github/workflows/verify.yml |
    grep -hoE 'version: "[0-9]+\.[0-9]+\.[0-9]+"' | head -n 1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(grep -A8 'goreleaser-action@' .github/workflows/release.yml |
    grep -hoE 'version: "[0-9]+\.[0-9]+\.[0-9]+"' | head -n 1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in README.md 'goreleaser/v2@v[0-9]+\.[0-9]+\.[0-9]+')"

# actionlint and ShellCheck: do CI and the local reproduction steps (README) name the same version?
pin actionlint \
  "$(version_in .github/workflows/verify.yml 'actionlint@v[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in README.md 'actionlint@v[0-9]+\.[0-9]+\.[0-9]+')"

pin shellcheck \
  "$(version_in .github/workflows/verify.yml 'SHELLCHECK_VERSION: v[0-9]+\.[0-9]+\.[0-9]+')" \
  "$(version_in README.md 'ShellCheck v[0-9]+\.[0-9]+\.[0-9]+')"

exit "$status"
