//go:build !xaispeech

package server

// xaiSpeechEnabled reports whether this build includes xAI STT/TTS.
const xaiSpeechEnabled = false

// registerXAISpeech is a no-op: xAI STT/TTS are built only with -tags xaispeech.
func registerXAISpeech(*Registry) {}

// registerOfflineSpeech is a no-op: offline.stt and offline.tts wrap the xAI speech adapters, which
// are built only with -tags xaispeech. offline.llm does not need them.
func registerOfflineSpeech(*Registry) {}
