package server

import (
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"govoice/core"
	"govoice/server/agent"
	"govoice/transport/ws"
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
	c.Audio.BackgroundDefault, c.Audio.AllowAgentBackground = def, allow
	m, err := loadBackgrounds(c, 24000)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: c, backgrounds: m, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestABackgroundIsChosenFromTheAgentThenTheServerDefaultAndNeverFromAPath(t *testing.T) {
	s := backgroundServer(t, true, "quiet")
	if u := s.backgroundFor(nil); u == nil || !near(level(t, u), 2000) { // -20 dB of 20000
		t.Fatalf("no flow settings: the server default (quiet, ~2000)")
	}
	pick := func(name string, gain *float64) *agent.AudioSettings {
		return &agent.AudioSettings{Background: &agent.BackgroundSettings{Name: name, GainDB: gain}}
	}
	if u := s.backgroundFor(pick("loud", nil)); u == nil || !near(level(t, u), 20000) {
		t.Fatal("the flow's sound with the catalogue's level")
	}
	if u := s.backgroundFor(pick("loud", ptrF(-20))); u == nil || !near(level(t, u), 2000) {
		t.Fatal("the flow's own gain_db overrides the catalogue's")
	}
	if u := s.backgroundFor(&agent.AudioSettings{OutputGainDB: ptrF(-3)}); u == nil || !near(level(t, u), 2000) {
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
	if none := backgroundServer(t, true, ""); none.backgroundFor(nil) != nil || none.backgroundFor(&agent.AudioSettings{OutputGainDB: ptrF(0)}) != nil {
		t.Fatal("no default and no flow choice: no background")
	}
}

func TestTheCeilingOffIgnoresAnAgentsBackgroundAtCallTime(t *testing.T) {
	s := backgroundServer(t, false, "quiet")
	u := s.backgroundFor(&agent.AudioSettings{Background: &agent.BackgroundSettings{Name: "loud"}})
	if u == nil || !near(level(t, u), 2000) {
		t.Fatal("with allow_flow_background off the flow's choice is ignored and the server default plays")
	}
	if none := backgroundServer(t, false, ""); none.backgroundFor(&agent.AudioSettings{Background: &agent.BackgroundSettings{Name: "loud"}}) != nil {
		t.Fatal("ceiling off and no default: silence")
	}
}

func near(got, want int16) bool { return got >= want-10 && got <= want+10 }

// K7: a route switches the bed to the arriving step's, keeps it for a step that names none, skips a switch to
// the sound already playing (each SetBackground restarts it) and restores the start step's resolved bed.
func TestRouteBackgroundSwitchesOnArrivalKeepsOtherwiseAndSkipsTheSame(t *testing.T) {
	s := backgroundServer(t, true, "quiet") // the start step names none: it plays the default, ~2000
	g := &CallAgent{Start: "front", Backgrounds: map[string]*agent.BackgroundSettings{
		"sales": {Name: "loud"}, "shop": {Name: "loud"}, "mute": {Name: ""},
	}}
	var sets []ws.Underlay
	hook := s.routeBackground(g, nil, func(u ws.Underlay) { sets = append(sets, u) })
	active := func(to string) { hook(core.RouteEvent{Kind: core.RouteActive, To: to}) }
	want := func(n int, what string) {
		t.Helper()
		if len(sets) != n {
			t.Fatalf("%s: %d SetBackground calls, want %d", what, len(sets), n)
		}
	}

	hook(core.RouteEvent{Kind: core.RoutePreparing, To: "sales"})
	hook(core.RouteEvent{Kind: core.RouteFailed, To: "sales"})
	want(0, "only an active route switches")
	active("sales")
	want(1, "arriving at a step with a sound")
	if !near(level(t, sets[0]), 20000) {
		t.Fatal("the arriving step's own sound")
	}
	active("shop")
	want(1, "a step naming the sound already playing must not restart it")
	active("plain")
	want(1, "a step naming none keeps the current sound")
	active("front")
	want(2, "back at the start step")
	if sets[1] == nil || !near(level(t, sets[1]), 2000) {
		t.Fatal("the start step's resolved bed (the server default) is restored")
	}
	active("mute")
	want(3, "a step asking for none")
	if sets[2] != nil {
		t.Fatal("none is a nil sound")
	}
	active("mute")
	want(3, "none again is not a switch")

	if s.routeBackground(&CallAgent{Start: "front"}, nil, func(ws.Underlay) {}) != nil {
		t.Fatal("no step names a sound: nothing to watch")
	}
}

// M2: a step's background the server refuses at call time (flow sounds turned off, or the sound dropped
// after publish) keeps the current sound: it must not fall back to the server default.
func TestARefusedStepBackgroundKeepsTheCurrentSoundInsteadOfTheServerDefault(t *testing.T) {
	for name, s := range map[string]*Server{"ceiling off": backgroundServer(t, false, "quiet"), "sound dropped": backgroundServer(t, true, "quiet")} {
		t.Run(name, func(t *testing.T) {
			g := &CallAgent{Start: "front", Backgrounds: map[string]*agent.BackgroundSettings{"b": {Name: "loud"}, "gone": {Name: "nope"}}}
			var sets int
			hook := s.routeBackground(g, &agent.AudioSettings{Background: &agent.BackgroundSettings{Name: ""}}, func(ws.Underlay) { sets++ })
			hook(core.RouteEvent{Kind: core.RouteActive, To: "gone"})
			if name == "ceiling off" {
				hook(core.RouteEvent{Kind: core.RouteActive, To: "b"})
			}
			if sets != 0 {
				t.Fatalf("a refused step background must keep the current sound (start asked for none), got %d SetBackground calls", sets)
			}
		})
	}
}
