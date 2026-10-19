#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
BIN_DIR="$ROOT_DIR/bin"
BIN="$BIN_DIR/herdr-drawer"
REPO="${HERDR_DRAWER_REPO:-iancharters/herdr-drawer}"

mkdir -p "$BIN_DIR"

detect_platform() {
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$os" in
    darwin|linux) ;;
    *) return 1 ;;
  esac
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    arm64|aarch64) arch="arm64" ;;
    *) return 1 ;;
  esac
  printf '%s_%s' "$os" "$arch"
}

download_url() {
  url="$1"
  dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$dest" "$url"
  else
    return 1
  fi
}

try_download_release() {
  if [ "${HERDR_DRAWER_FORCE_GO_BUILD:-0}" = "1" ]; then
    return 1
  fi
  platform="$(detect_platform)" || return 1
  archive="herdr-drawer_${platform}.tar.gz"
  version="$(sed -n 's/^version *= *"\([^"]*\)".*/\1/p' "$ROOT_DIR/herdr-plugin.toml" | head -n 1)"

  tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/herdr-drawer-build.XXXXXX")"
  trap 'rm -rf "$tmp_dir"' EXIT

  downloaded=0
  if [ -n "$version" ]; then
    if download_url "https://github.com/${REPO}/releases/download/v${version}/${archive}" "$tmp_dir/$archive" 2>/dev/null; then
      downloaded=1
    fi
  fi
  if [ "$downloaded" -eq 0 ]; then
    if download_url "https://github.com/${REPO}/releases/latest/download/${archive}" "$tmp_dir/$archive" 2>/dev/null; then
      downloaded=1
    fi
  fi
  if [ "$downloaded" -eq 0 ]; then
    rm -rf "$tmp_dir"
    trap - EXIT
    return 1
  fi

  if tar -xzf "$tmp_dir/$archive" -C "$tmp_dir" herdr-drawer 2>/dev/null && [ -f "$tmp_dir/herdr-drawer" ]; then
    mv "$tmp_dir/herdr-drawer" "$BIN"
    chmod +x "$BIN"
    rm -rf "$tmp_dir"
    trap - EXIT
    return 0
  fi

  rm -rf "$tmp_dir"
  trap - EXIT
  return 1
}

try_go_build() {
  go_bin="${GO_BIN:-}"
  if [ -z "$go_bin" ]; then
    if command -v go >/dev/null 2>&1; then
      go_bin="$(command -v go)"
    elif [ -x "/opt/homebrew/bin/go" ]; then
      go_bin="/opt/homebrew/bin/go"
    elif [ -x "/usr/local/go/bin/go" ]; then
      go_bin="/usr/local/go/bin/go"
    elif [ -x "${HOME:-}/go/bin/go" ]; then
      go_bin="${HOME}/go/bin/go"
    else
      return 1
    fi
  fi
  (cd "$ROOT_DIR" && "$go_bin" build -ldflags="-s -w" -o "$BIN" .)
}

if try_download_release; then
  exit 0
fi

if try_go_build; then
  exit 0
fi

echo "herdr-drawer: failed to download prebuilt release binary from https://github.com/${REPO}/releases and 'go' was not found to build from source" >&2
exit 1
