package server

import (
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"govoice/server/flow"
)

// constWAV writes a 100 ms 24 kHz mono WAV whose every sample is v and returns its path.
func constWAV(t *testing.T, name string, v int16) string {
	t.Helper()
	pcm := make([]byte, 2*2400)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(v))
	}
	hdr := []byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data")
	hdr = append(hdr, byte(len(pcm)), byte(len(pcm)>>8), 0, 0)
	path := filepath.Join(t.TempDir(), name+".wav")
	if err := os.WriteFile(path, append(hdr, pcm...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// level is the first sample a background plays, with nothing else mixed in.
func level(t *testing.T, u interface{ Mix(dst, src []byte) }) int16 {
	t.Helper()
	b := make([]byte, 2)
	u.Mix(b, nil)
	return int16(binary.LittleEndian.Uint16(b))
}

func backgroundServer(t *testing.T, allow bool, def string) *Server {
	t.Helper()
	c := DefaultConfig(ProfileProduction)
	c.Audio.Backgrounds = map[string]CueConfig{
		"loud":  {File: constWAV(t, "loud", 20000), GainDB: ptrF(0)},
		"quiet": {File: constWAV(t, "quiet", 20000), GainDB: ptrF(-20)},
	}
	c.Audio.BackgroundDefault, c.Audio.AllowFlowBackground = def, allow
	m, err := loadBackgrounds(c, 24000)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: c, backgrounds: m, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestABackgroundIsChosenFromTheFlowThenTheServerDefaultAndNeverFromAPath(t *testing.T) {
	s := backgroundServer(t, true, "quiet")
	if u := s.backgroundFor(nil); u == nil || !near(level(t, u), 2000) { // -20 dB of 20000
		t.Fatalf("no flow settings: the server default (quiet, ~2000)")
	}
	pick := func(name string, gain *float64) *flow.AudioSettings {
		return &flow.AudioSettings{Background: &flow.BackgroundSettings{Name: name, GainDB: gain}}
	}
	if u := s.backgroundFor(pick("loud", nil)); u == nil || !near(level(t, u), 20000) {
		t.Fatal("the flow's sound with the catalogue's level")
	}
	if u := s.backgroundFor(pick("loud", ptrF(-20))); u == nil || !near(level(t, u), 2000) {
		t.Fatal("the flow's own gain_db overrides the catalogue's")
	}
	if u := s.backgroundFor(&flow.AudioSettings{OutputGainDB: ptrF(-3)}); u == nil || !near(level(t, u), 2000) {
		t.Fatal("a flow that sets other audio members keeps the server default background")
	}
	for _, bad := range []string{"../../etc/passwd", "nope"} {
		if u := s.backgroundFor(pick(bad, nil)); u == nil || !near(level(t, u), 2000) {
			t.Fatalf("name %q must fall back to the server default and never open a file", bad)
		}
	}
	if u := s.backgroundFor(pick("", nil)); u != nil {
		t.Fatal("a flow that asks for none plays none, even when the server has a default")
	}
	if none := backgroundServer(t, true, ""); none.backgroundFor(nil) != nil || none.backgroundFor(&flow.AudioSettings{OutputGainDB: ptrF(0)}) != nil {
		t.Fatal("no default and no flow choice: no background")
	}
}

func TestTheCeilingOffIgnoresAFlowsBackgroundAtCallTime(t *testing.T) {
	s := backgroundServer(t, false, "quiet")
	u := s.backgroundFor(&flow.AudioSettings{Background: &flow.BackgroundSettings{Name: "loud"}})
	if u == nil || !near(level(t, u), 2000) {
		t.Fatal("with allow_flow_background off the flow's choice is ignored and the server default plays")
	}
	if none := backgroundServer(t, false, ""); none.backgroundFor(&flow.AudioSettings{Background: &flow.BackgroundSettings{Name: "loud"}}) != nil {
		t.Fatal("ceiling off and no default: silence")
	}
}

func near(got, want int16) bool { return got >= want-10 && got <= want+10 }
