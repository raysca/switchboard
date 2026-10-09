#!/usr/bin/env bash
# ONNX Runtime for a fresh clone (the library is git-ignored: *.dylib, *.so).
#   scripts/fetch-ort.sh check   exit 1 with instructions when the library is missing (make build)
#   scripts/fetch-ort.sh fetch   download the pinned release, verify sha256 and install it (make ort)
# Install locations are the ones vad.ORTLibPath searches:
#   macOS arm64  third_party/onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.dylib
#   Linux        lib/libonnxruntime.so   (same pinned hashes as deploy/Dockerfile)
# GOVOICE_ORT_LIB (an existing library path) also satisfies `check`.
set -euo pipefail
cd "$(dirname "$0")/.."

V=1.29.0
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)
    asset="onnxruntime-osx-arm64-$V"
    sha="" # ponytail: no pinned macOS hash yet; fetch prints the computed one. Pin it after the first verified download.
    dest="third_party/$asset/lib/libonnxruntime.dylib"
    ;;
  Linux-x86_64)
    asset="onnxruntime-linux-x64-$V"
    sha="c3fddc4f139a045b0c4902c57410f0694f1c2fdf9b6939fbe38b1aeae7cd14ba"
    dest="lib/libonnxruntime.so"
    ;;
  Linux-aarch64 | Linux-arm64)
    asset="onnxruntime-linux-aarch64-$V"
    sha="e1799098ebc054b370f6176a450f158720f297818c613e5dc99b92e2ec82346f"
    dest="lib/libonnxruntime.so"
    ;;
  *)
    echo "fetch-ort: unsupported platform $(uname -s)-$(uname -m); set GOVOICE_ORT_LIB to a libonnxruntime $V" >&2
    exit 1
    ;;
esac

case "${1:-check}" in
  check)
    if [[ -f "$dest" || -f "${GOVOICE_ORT_LIB:-/nonexistent}" ]]; then exit 0; fi
    echo "ONNX Runtime $V is missing ($dest)." >&2
    echo "It is git-ignored; run 'make ort' to download the pinned release, or set GOVOICE_ORT_LIB." >&2
    exit 1
    ;;
  fetch)
    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT
    curl -fsSL -o "$tmp/ort.tgz" "https://github.com/microsoft/onnxruntime/releases/download/v$V/$asset.tgz"
    got="$( (sha256sum "$tmp/ort.tgz" 2>/dev/null || shasum -a 256 "$tmp/ort.tgz") | cut -d' ' -f1)"
    if [[ -n "$sha" && "$got" != "$sha" ]]; then
      echo "fetch-ort: sha256 mismatch for $asset.tgz: got $got, want $sha" >&2
      exit 1
    fi
    [[ -n "$sha" ]] || echo "fetch-ort: WARNING no pinned sha256 for $asset.tgz (got $got)" >&2
    tar -xzf "$tmp/ort.tgz" -C "$tmp"
    mkdir -p "$(dirname "$dest")"
    if [[ "$dest" == third_party/* ]]; then
      cp -R "$tmp/$asset/lib/." "third_party/$asset/lib/"
    else
      cp "$tmp/$asset/lib/libonnxruntime.so" "$dest"
    fi
    echo "installed $dest"
    ;;
  *)
    echo "usage: $0 check|fetch" >&2
    exit 2
    ;;
esac
