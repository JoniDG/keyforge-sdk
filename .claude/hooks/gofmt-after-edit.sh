#!/usr/bin/env bash
#
# Hook: after Edit/Write, if the file is .go, run gofmt + goimports.

set -euo pipefail

payload=$(cat)
file=$(printf '%s' "$payload" | sed -n 's/.*"file_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')

if [[ -z "$file" ]]; then
  exit 0
fi

if [[ "$file" != *.go ]]; then
  exit 0
fi

if [[ ! -f "$file" ]]; then
  exit 0
fi

gofmt -w "$file" 2>/dev/null || true

if command -v goimports >/dev/null 2>&1; then
  goimports -w "$file" 2>/dev/null || true
fi

exit 0
