#!/usr/bin/env bash
# Compatibility entry point. All scenarios and resource management live in Go.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
# Environment paths are relative to the caller, not the repository directory below.
export CLRND_E2E_CALLER_DIR="$PWD"
case "${1:-}" in
  "") ;;
  --cleanup-orphans) export CLRND_E2E_CLEANUP_ORPHANS=1; shift ;;
  *) echo "usage: $0 [--cleanup-orphans]" >&2; exit 1 ;;
esac
if [ "$#" -ne 0 ]; then
  echo "usage: $0 [--cleanup-orphans]" >&2
  exit 1
fi
cd "$here/../.."
exec go test -tags=e2e -count=1 -timeout=0 -v ./test/e2e -run '^TestE2E$'
