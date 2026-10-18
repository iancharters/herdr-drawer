#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$DIR/bin/herdr-drawer"

if [ ! -x "$BIN" ] || [ "$DIR/main.go" -nt "$BIN" ] || [ "$DIR/session.go" -nt "$BIN" ]; then
  mkdir -p "$DIR/bin"
  GO_BIN="${GO_BIN:-}"
  if [ -z "$GO_BIN" ]; then
    if command -v go >/dev/null 2>&1; then
      GO_BIN="$(command -v go)"
    elif [ -x "/opt/homebrew/bin/go" ]; then
      GO_BIN="/opt/homebrew/bin/go"
    elif [ -x "/usr/local/go/bin/go" ]; then
      GO_BIN="/usr/local/go/bin/go"
    else
      echo "herdr-drawer: go binary not found to compile plugin" >&2
      exit 127
    fi
  fi
  (cd "$DIR" && "$GO_BIN" build -o "$BIN" .)
fi

exec "$BIN" "$@"
