package server

import (
	"cmp"
	"slices"
	"time"

	coreturn "govoice/core/turn"
	"govoice/server/flow"
)

// effectiveMaxMute is the cap on one continuous mute: turn.max_mute, or the library's default.
func (t TurnConfig) effectiveMaxMute() time.Duration {
	return cmp.Or(t.MaxMute, coreturn.DefaultMaxMute)
}

// effectiveSpeechTimeout is the resume window after the VAD stop: turn.speech_timeout, or the library's default.
func (t TurnConfig) effectiveSpeechTimeout() time.Duration {
	return cmp.Or(t.SpeechTimeout, coreturn.DefaultUserSpeechTimeout)
}

// forCall is the server's turn settings with a flow's choices laid over them, member by member. The
// server's config is shared by every call, so the result never aliases its slices, and a flow can only
// shorten the mute cap.
func (t TurnConfig) forCall(f *flow.TurnSettings) TurnConfig {
	t.Mute = slices.Clone(t.Mute)
	if f == nil {
		return t
	}
	if f.MinWords != nil {
		t.MinWords = *f.MinWords
	}
	if f.Mute != nil {
		t.Mute = slices.Clone(*f.Mute)
	}
	if f.MaxMuteMS != nil {
		t.MaxMute = min(t.effectiveMaxMute(), time.Duration(*f.MaxMuteMS)*time.Millisecond)
	}
	return t
}
