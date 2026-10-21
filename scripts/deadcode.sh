#!/usr/bin/env bash
# Fails when a function that no binary, example or tool reaches is not allowlisted.
# Tool: golang.org/x/tools/cmd/deadcode, pinned (bump deliberately). Run: make deadcode.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
out=$(go run golang.org/x/tools/cmd/deadcode@v0.52.0 -tags xaispeech -test=false \
  ./cmd/... ./examples/... ./tools/... ./offline/cmd/...)
# "path/file.go:12:6: unreachable func: Name" -> "path/file.go Name" (line moves are not new findings)
norm=$(printf '%s\n' "$out" | sed -nE 's/^([^:]+):[0-9]+:[0-9]+: unreachable func: (.+)$/\1 \2/p' | sort -u)
allow=$(sed -E -e '/^[[:space:]]*(#|$)/d' -e 's/[[:space:]]+#.*$//' scripts/deadcode-allow.txt)
new=$norm
if [ -n "$allow" ]; then new=$(printf '%s\n' "$norm" | grep -vE -f <(printf '%s\n' "$allow") || true); fi
new=$(printf '%s\n' "$new" | sed '/^$/d')
if [ -n "$new" ]; then
  echo "deadcode: unreachable and not in scripts/deadcode-allow.txt (delete it, move it to a _test.go file, or allowlist it with a reason):"
  printf '%s\n' "$new"; exit 1
fi
echo "deadcode: OK ($(printf '%s\n' "$norm" | sed '/^$/d' | wc -l | tr -d ' ') allowed)"
