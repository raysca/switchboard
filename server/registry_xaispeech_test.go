//go:build xaispeech

package server

import (
	"testing"

	"govoice/providers/xai"
)

// xAI TTS streams unless providers.xai_tts.disable_streaming is set.
func TestXAITTSFactoryStreamsUnlessDisabled(t *testing.T) {
	reg := NewRegistry()
	f, ok := reg.factories["xai.tts"]
	if !ok {
		t.Fatal("xai.tts is not registered in this build")
	}
	for _, disable := range []bool{false, true} {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Providers.XAITTS.DisableStreaming = disable
		p, err := f(cfg, StageSpec{Voice: "eve", Secret: []byte("xai-test-key-0123456789")})
		if err != nil {
			t.Fatal(err)
		}
		_, streaming := p.(*xai.StreamingTTSProcessor)
		_, unary := p.(*xai.TTSProcessor)
		if streaming == disable || unary != disable {
			t.Errorf("disable_streaming=%t built %T", disable, p)
		}
		if cl, ok := p.(interface{ Close() error }); ok {
			_ = cl.Close()
		}
	}
}
