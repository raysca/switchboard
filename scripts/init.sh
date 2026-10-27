#!/usr/bin/env bash
# Installs the pinned `just` task runner used by ./dev.
#
# The version and SHA-256 values below are the release's published SHA256SUMS:
# https://github.com/casey/just/releases/download/1.59.0/SHA256SUMS
#
# We download the tarball, verify it, and only then install the binary. There is no
# `curl | sh`, no sudo, and no PATH change: ./dev finds .tools/bin/just directly.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
JUST_VERSION=1.59.0
JUST_BIN="$ROOT/.tools/bin/just"
RELEASE="https://github.com/casey/just/releases/download/${JUST_VERSION}"
TOOLS="$ROOT/.tools"

die() { echo "init: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required to install just"; }

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)
    ASSET="just-${JUST_VERSION}-aarch64-apple-darwin.tar.gz"
    SHA256="6574cea774ed4cae12e24b6aa11857de33e80dab4667cf628fd5840df96a2a9d"
    ;;
  Darwin-x86_64)
    ASSET="just-${JUST_VERSION}-x86_64-apple-darwin.tar.gz"
    SHA256="036ead9df9bc31dbae9934ae3b4236ed124d0bafcffd33439460ae084d5c9cb7"
    ;;
  Linux-x86_64)
    ASSET="just-${JUST_VERSION}-x86_64-unknown-linux-musl.tar.gz"
    SHA256="409044617b6f06517c1035e55ca38c8c856ff8b650e41bc2aa1ef82d5903a23e"
    ;;
  Linux-aarch64|Linux-arm64)
    ASSET="just-${JUST_VERSION}-aarch64-unknown-linux-musl.tar.gz"
    SHA256="23871b1702bfd550fb5f901778fd4290ecf2ee034a45a889fbe96d89b188086d"
    ;;
  *)
    cat >&2 <<EOF
init: unsupported platform $(uname -s)/$(uname -m).
Supported: macOS x86_64/arm64, Linux x86_64/aarch64.
Install just ${JUST_VERSION} yourself (e.g. \`cargo install just --version ${JUST_VERSION}\`)
and put it at .tools/bin/just, then run ./dev.
EOF
    exit 1
    ;;
esac

if [[ -x "$JUST_BIN" ]]; then
  have="$("$JUST_BIN" --version 2>/dev/null || true)"
  if [[ "$have" == "just ${JUST_VERSION}" ]]; then
    echo "just ${JUST_VERSION} is already installed at ${JUST_BIN#"$ROOT"/}"
    echo "run ./dev --list for the commands"
    exit 0
  fi
fi

need curl
need tar
need mktemp
need uname
need install

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"; rm -f "$JUST_BIN.new"' EXIT

echo "init: downloading just ${JUST_VERSION} (${ASSET})"
curl -fsSL --retry 3 --retry-delay 2 -o "$tmp/just.tar.gz" "${RELEASE}/${ASSET}" ||
  die "download failed: ${RELEASE}/${ASSET}"

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp/just.tar.gz" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp/just.tar.gz" | awk '{print $1}')"
else
  die "neither sha256sum nor shasum is available to verify the download"
fi
[[ "$actual" == "$SHA256" ]] || die "checksum mismatch for ${ASSET}: got ${actual}, want ${SHA256}"

tar -xzf "$tmp/just.tar.gz" -C "$tmp" just ||
  die "the release archive does not contain just"
mkdir -p "$TOOLS/bin"
# Install only after verification; a failed run leaves the previous binary untouched.
install -m 0755 "$tmp/just" "$JUST_BIN.new"
mv -f "$JUST_BIN.new" "$JUST_BIN"

echo "just ${JUST_VERSION} installed at ${JUST_BIN#"$ROOT"/}"
cat <<'EOF'

Start here:
  ./dev                 show common commands and the recipe list
  ./dev --list          every task with a one-line description
  ./dev help deploy     deploy a server or a demo
  ./dev help demo       start, seed, reset or wipe a demo
  ./dev doctor          check local prerequisites
EOF
