#!/usr/bin/env sh
# axup installer.
#
#   curl -fsSL https://raw.githubusercontent.com/AxGrid/axup/master/install.sh | sh
#
# Detects OS / arch, downloads the matching prebuilt binary from the latest
# GitHub Release, and drops it into $AXUP_BIN_DIR (default: ~/.local/bin).
# If no prebuilt asset is available it falls back to `go install` from source.
#
# Env vars:
#   AXUP_VERSION    Tag to install (e.g. v0.0.4 or master). Default: latest release.
#   AXUP_BIN_DIR    Install directory. Default: $HOME/.local/bin.
#   AXUP_REPO       GitHub owner/repo. Default: AxGrid/axup.

set -eu

REPO="${AXUP_REPO:-AxGrid/axup}"
MODULE="github.com/axgrid/axup/cmd/axup"
BIN_DIR="${AXUP_BIN_DIR:-$HOME/.local/bin}"

err() { printf 'axup install: %s\n' "$*" >&2; }

uname_s=$(uname -s | tr '[:upper:]' '[:lower:]')
uname_m=$(uname -m)
case "$uname_s" in
  darwin) os=darwin ;;
  linux)  os=linux ;;
  *) err "unsupported OS '$uname_s'"; exit 1 ;;
esac
case "$uname_m" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) err "unsupported arch '$uname_m'"; exit 1 ;;
esac

mkdir -p "$BIN_DIR"

resolve_latest() {
  curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
    | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' \
    | head -n1
}

version="${AXUP_VERSION:-}"
if [ -z "$version" ]; then
  version=$(resolve_latest || true)
fi

try_prebuilt() {
  [ -n "$version" ] || return 1
  asset="axup-$os-$arch"
  url="https://github.com/$REPO/releases/download/$version/$asset"
  tmp=$(mktemp)
  printf 'Downloading %s\n' "$url"
  if ! curl -fsSL "$url" -o "$tmp"; then
    rm -f "$tmp"
    return 1
  fi
  chmod +x "$tmp"
  mv "$tmp" "$BIN_DIR/axup"
  return 0
}

try_go_install() {
  if ! command -v go >/dev/null 2>&1; then
    return 1
  fi
  target="${AXUP_VERSION:-master}"
  tmpbin=$(mktemp -d)
  trap 'rm -rf "$tmpbin"' EXIT INT TERM
  printf 'Running: go install %s@%s\n' "$MODULE" "$target"
  GOBIN="$tmpbin" GOFLAGS=-trimpath go install "$MODULE@$target"
  install -m 0755 "$tmpbin/axup" "$BIN_DIR/axup"
}

if try_prebuilt; then
  :
elif try_go_install; then
  :
else
  err "no prebuilt asset for $os/$arch at $version and 'go' not in PATH"
  err "install Go (https://go.dev/dl/) and re-run, or clone the repo and use 'make install'"
  exit 1
fi

printf '\nInstalled: %s\n' "$BIN_DIR/axup"
"$BIN_DIR/axup" version || true

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *)
    printf '\nNote: %s is not in your PATH. Add this to your shell rc:\n' "$BIN_DIR"
    printf '  export PATH="%s:$PATH"\n' "$BIN_DIR"
    ;;
esac
