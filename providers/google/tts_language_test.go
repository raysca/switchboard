package google

import "testing"

func TestTTSLanguageFollowsVoice(t *testing.T) {
	for _, tc := range []struct{ voice, fallback, want string }{
		{"en-GB-Neural2-A", "en-US", "en-GB"},
		{"cmn-CN-Wavenet-A", "en-US", "cmn-CN"},
		{"Kore", "de-DE", "de-DE"},
		{"Kore", "", "en-US"},
	} {
		if got := voiceLanguage(tc.voice, tc.fallback); got != tc.want {
			t.Errorf("voiceLanguage(%q, %q) = %q, want %q", tc.voice, tc.fallback, got, tc.want)
		}
	}
}
