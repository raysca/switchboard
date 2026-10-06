#!/usr/bin/env bash
# Regenerates the fake-microphone WAVs: macOS `say` speech, 16 kHz mono PCM16,
# with 1 s of silence before and 20 s after: VAD sees a clean start/end of speech
# and, because Chromium's --use-file-for-fake-audio-capture LOOPS the file, the
# bot can answer before the question repeats (a repeat is a barge-in).
set -euo pipefail
cd "$(dirname "$0")/../fixtures"
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT

make_wav() { # <out.wav> <text>
  say -o "$tmp/speech.aiff" "$2"
  ffmpeg -loglevel error -y -i "$tmp/speech.aiff" \
    -af "adelay=1000:all=1,apad=pad_dur=20" -ar 16000 -ac 1 -c:a pcm_s16le "$1"
}

make_wav voice-debug.wav "What is two plus two?"
make_wav voice-custom.wav "What is the capital of France?"
ls -l *.wav
