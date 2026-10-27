#!/usr/bin/env bash
# Failure-path tests for scripts/init.sh. Runs against a temporary copy with fake
# uname/curl/tar/shasum commands, so no network and no real .tools install is touched.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

repo="$tmp/repo"
mkdir -p "$repo/scripts" "$repo/.tools/bin" "$tmp/bin"
cp "$ROOT/scripts/init.sh" "$repo/scripts/init.sh"
chmod +x "$repo/scripts/init.sh"

cat > "$tmp/bin/uname" <<'FAKE'
#!/usr/bin/env bash
case "${1:-}" in
  -s) echo "${FAKE_OS:-Darwin}" ;;
  -m) echo "${FAKE_ARCH:-arm64}" ;;
  *) echo "unknown" ;;
esac
FAKE

cat > "$tmp/bin/curl" <<'FAKE'
#!/usr/bin/env bash
out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[[ -n "$out" ]] || exit 2
[[ "${FAKE_CURL_FAIL:-0}" == 1 ]] && exit 22
printf 'fake-archive' > "$out"
FAKE

cat > "$tmp/bin/tar" <<'FAKE'
#!/usr/bin/env bash
out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -C) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[[ -n "$out" ]] || exit 2
cat > "$out/just" <<'JUST'
#!/usr/bin/env bash
if [[ "${1:-}" == "--version" ]]; then echo "just 1.59.0"; fi
JUST
chmod +x "$out/just"
FAKE

cat > "$tmp/bin/shasum" <<'FAKE'
#!/usr/bin/env bash
if [[ "${1:-}" == "-a" ]]; then shift 2; fi
printf '%s  %s\n' "${FAKE_SHA:-bad}" "${1:-}"
FAKE

cat > "$tmp/bin/sha256sum" <<'FAKE'
#!/usr/bin/env bash
printf '%s  %s\n' "${FAKE_SHA:-bad}" "${1:-}"
FAKE

chmod +x "$tmp/bin/"*
run() {
  ( cd "$repo" && PATH="$tmp/bin:$PATH" "$@" )
}
expect_rc() {
  local want="$1"; shift
  local rc=0
  "$@" >/dev/null 2>&1 || rc=$?
  [[ "$rc" -eq "$want" ]] || { echo "init-test: expected exit $want, got $rc for: $*" >&2; exit 1; }
}

export FAKE_SHA=6574cea774ed4cae12e24b6aa11857de33e80dab4667cf628fd5840df96a2a9d

# A platform without a pinned asset must fail and leave no binary.
expect_rc 1 run env FAKE_OS=Plan9 ./scripts/init.sh
[[ ! -e "$repo/.tools/bin/just" ]] || { echo "init-test: unsupported platform installed a binary" >&2; exit 1; }

# A checksum mismatch must fail and leave the previous binary untouched.
mkdir -p "$repo/.tools/bin"
printf '#!/usr/bin/env bash\necho "existing"\n' > "$repo/.tools/bin/just"
chmod +x "$repo/.tools/bin/just"
expect_rc 1 run env FAKE_SHA=deadbeef ./scripts/init.sh
[[ "$("$repo/.tools/bin/just")" == "existing" ]] || { echo "init-test: checksum failure replaced the existing binary" >&2; exit 1; }

# A successful install and an idempotent second run.
rm -f "$repo/.tools/bin/just"
expect_rc 0 run ./scripts/init.sh
[[ "$("$repo/.tools/bin/just" --version)" == "just 1.59.0" ]] || { echo "init-test: installed binary is wrong" >&2; exit 1; }
expect_rc 0 run ./scripts/init.sh

# A failed download must not install anything.
rm -rf "$repo/.tools"
expect_rc 1 run env FAKE_CURL_FAIL=1 ./scripts/init.sh
[[ ! -e "$repo/.tools/bin/just" ]] || { echo "init-test: failed download installed a binary" >&2; exit 1; }

echo "init-test: unsupported platform, checksum mismatch, failed download, install and idempotency pass"
