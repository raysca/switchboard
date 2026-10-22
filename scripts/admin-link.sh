#!/usr/bin/env bash
# Prints a magic link that signs the admin UI in without pasting the management key:
#   <admin UI URL>/#key=<management key>
# The key sits in the URL fragment, which the browser never sends to any server and the UI removes from
# the address bar as soon as it has read it. Treat the link like the key itself: do not paste it into
# chat or tickets, and prefer a private terminal.
#
#   GOVOICE_MANAGEMENT_API_KEY=... scripts/admin-link.sh https://switchboard.example.com
#   gcloud secrets versions access latest --secret=SWITCHBOARD_MANAGEMENT_API_KEY | scripts/admin-link.sh https://...
#   make admin-link URL=https://switchboard.example.com          (key from the environment)
#   make admin-link                                              (the local dev stack's UI and key)
set -euo pipefail
export LC_ALL=C   # byte-wise: a non-ASCII key is encoded as its UTF-8 bytes, and [A-Za-z] means ASCII

die() { echo "admin-link: $*" >&2; exit 2; }

# Percent-encode everything but unreserved characters, so a key with & = + % # or a space survives.
urlencode() {
  local s="$1" i c out=""
  for (( i = 0; i < ${#s}; i++ )); do
    c="${s:i:1}"
    case "$c" in
      [A-Za-z0-9._~-]) out+="$c" ;;
      *) out+="$(printf '%%%02X' $(( $(printf '%d' "'$c") & 255 )))" ;;   # & 255: a byte above 127 would sign-extend
    esac
  done
  printf '%s' "$out"
}

[[ $# -eq 1 ]] || die "usage: scripts/admin-link.sh <admin UI URL>   (key from GOVOICE_MANAGEMENT_API_KEY or stdin)"
url="${1%/}"
case "$url" in http://*|https://*) ;; *) die "the URL must start with http:// or https://" ;; esac
key="${GOVOICE_MANAGEMENT_API_KEY:-}"
if [[ -z "$key" && ! -t 0 ]]; then IFS= read -r key || true; fi
key="${key//$'\r'/}"
[[ -n "$key" ]] || die "no key: set GOVOICE_MANAGEMENT_API_KEY or pipe it in"
printf '%s/#key=%s\n' "$url" "$(urlencode "$key")"
