package server

import (
	"sync"

	"govoice/core"
	"govoice/server/agent"
	"govoice/transport/ws"
)

// bed is a resolved background: the catalogue sound with the level and looping a call plays it at.
// Comparable, so "the same sound at the same level" is ==.
type bed struct {
	name   string
	b      background
	gainDB float64
	loop   bool
}

// underlay starts a fresh cursor over the bed (each one restarts the sound from its beginning).
func (e bed) underlay() ws.Underlay { return e.b.underlay(&e.gainDB, &e.loop) }

// resolveBed is the sound a call plays under the voice, ok false for none: the one its flow names, else the
// server default (audio.background_default); a flow naming "" means none, default or not. A flow's choice is honoured only for a catalogue name and
// only while the operator allows flow backgrounds; publish and readiness already refuse anything else,
// so reaching the fallback means the configuration changed after the flow was published. The name is
// only ever a key into the catalogue: a flow cannot make the server open a file.
func (s *Server) resolveBed(f *agent.AudioSettings) (bed, bool) {
	if f != nil && f.Background != nil {
		if f.Background.Name == "" {
			return bed{}, false // the flow asked for none, whatever the server's default is
		}
		b, ok := s.backgrounds[f.Background.Name]
		switch {
		case !s.cfg.Audio.AllowAgentBackground:
			s.logger.Warn("flow background ignored: audio.allow_flow_background is off", "sound", f.Background.Name)
		case !ok:
			s.logger.Warn("flow background ignored: not in audio.backgrounds", "sound", f.Background.Name)
		default:
			e := bed{name: f.Background.Name, b: b, gainDB: b.gainDB, loop: b.loop}
			if g := f.Background.GainDB; g != nil {
				e.gainDB = *g
			}
			if l := f.Background.Loop; l != nil {
				e.loop = *l
			}
			return e, true
		}
	}
	if b, ok := s.backgrounds[s.cfg.Audio.BackgroundDefault]; ok {
		return bed{name: s.cfg.Audio.BackgroundDefault, b: b, gainDB: b.gainDB, loop: b.loop}, true
	}
	return bed{}, false
}

// backgroundFor is the sound a call starts with, or nil (see resolveBed).
func (s *Server) backgroundFor(f *agent.AudioSettings) ws.Underlay {
	if e, ok := s.resolveBed(f); ok {
		return e.underlay()
	}
	return nil
}

// stepBackgroundUsable says whether the server will play a non-start step's background now. A step
// that asks for none is usable; one the server refuses is skipped, not replaced by the default.
func (s *Server) stepBackgroundUsable(b *agent.BackgroundSettings) bool {
	if b.Name == "" {
		return true
	}
	_, known := s.backgrounds[b.Name]
	if !s.cfg.Audio.AllowAgentBackground || !known {
		s.logger.Warn("step background ignored: the step keeps the current sound", "sound", b.Name, "allowed", s.cfg.Audio.AllowAgentBackground, "known", known)
		return false
	}
	return true
}

// handoffBackground returns the OnHandoff hook that switches the bed when a route makes a step active, or nil
// when no step names a background (nothing would ever change). A step that names none keeps the playing
// sound; the start step always has an entry, its resolved bed (server default included), so a return to it
// restores what the call began with. A switch to the bed already playing is skipped: set restarts a sound.
func (s *Server) handoffBackground(g *CallAgent, startAudio *agent.AudioSettings, set func(ws.Underlay)) func(core.HandoffEvent) {
	if g == nil || len(g.Backgrounds) == 0 {
		return nil
	}
	type choice struct {
		e  bed
		on bool
	}
	choices := make(map[string]choice, len(g.Backgrounds)+1)
	for id, b := range g.Backgrounds {
		if id != g.Start && !s.stepBackgroundUsable(b) {
			continue // refused now (ceiling off, sound dropped): the step keeps the current sound
		}
		e, on := s.resolveBed(&agent.AudioSettings{Background: b})
		choices[id] = choice{e, on}
	}
	start, on := s.resolveBed(startAudio)
	choices[g.Start] = choice{start, on}
	var mu sync.Mutex
	playing := choices[g.Start]
	return func(ev core.HandoffEvent) {
		if ev.Kind != core.HandoffActive {
			return
		}
		next, ok := choices[ev.To]
		mu.Lock()
		same := !ok || next == playing
		if !same {
			playing = next
		}
		mu.Unlock()
		if same {
			return
		}
		if next.on {
			set(next.e.underlay())
		} else {
			set(nil)
		}
	}
}
