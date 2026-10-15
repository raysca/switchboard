package server

import (
	"govoice/server/flow"
	"govoice/transport/ws"
)

// backgroundFor is the sound a call plays under the voice, or nil: the one its flow names, else the
// server default (audio.background_default). A flow's choice is honoured only for a catalogue name and
// only while the operator allows flow backgrounds; publish and readiness already refuse anything else,
// so reaching the fallback means the configuration changed after the flow was published. The name is
// only ever a key into the catalogue: a flow cannot make the server open a file.
func (s *Server) backgroundFor(f *flow.AudioSettings) ws.Underlay {
	if f != nil && f.Background != nil {
		b, ok := s.backgrounds[f.Background.Name]
		switch {
		case !s.cfg.Audio.AllowFlowBackground:
			s.logger.Warn("flow background ignored: audio.allow_flow_background is off", "sound", f.Background.Name)
		case !ok:
			s.logger.Warn("flow background ignored: not in audio.backgrounds", "sound", f.Background.Name)
		default:
			return b.underlay(f.Background.GainDB, f.Background.Loop)
		}
	}
	if b, ok := s.backgrounds[s.cfg.Audio.BackgroundDefault]; ok {
		return b.underlay(nil, nil)
	}
	return nil
}
