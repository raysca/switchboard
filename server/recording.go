package server

import (
	"fmt"
	"log/slog"
	"time"

	"govoice/core"
	"govoice/core/record"
	"govoice/server/storage"
	"govoice/transport/ws"
)

// attachRecording starts recording the call when the server enables it AND the
// flow opted in (with disclosure confirmed). It returns a stop func that
// finalizes the file; stop is nil when the call is not recorded. Recording can
// never fail the call: sink errors only cost the recording.
func (s *Server) attachRecording(call CallSpec, flowID string, sessionID uint64, conn *ws.Connection, session *core.Session) (stop func()) {
	if s.recSink == nil || !s.cfg.Recording.Enabled || !call.Recording {
		return nil
	}
	started := time.Now()
	safeFlow := flowID
	if !record.ValidID(safeFlow) || len(safeFlow) > 64 || !storage.IsID(safeFlow) {
		safeFlow = "flow"
	}
	sid := fmt.Sprint(sessionID)
	rec := record.New(s.recSink, record.Meta{
		ID:        fmt.Sprintf("%s-%d-%d", safeFlow, sessionID, started.Unix()),
		SessionID: sid, FlowID: flowID, CallID: sid,
	}, record.Config{MaxDuration: s.cfg.Recording.MaxDuration, MaxBytes: s.cfg.Recording.MaxBytes})
	conn.SetAudioTap(rec)

	// Transcript turns ride the existing frame hook. OnFrame may see a frame at
	// several pipeline hops, so count each frame once. The assistant side is
	// the committed message (only the heard prefix after a barge-in), matching
	// the bot track and the stored turn history.
	prev := session.OnFrame
	var lastUser *core.UserMessageFrame
	var lastBot *core.AssistantMessageFrame
	session.OnFrame = func(f core.Frame, dir core.Direction) {
		if prev != nil {
			prev(f, dir)
		}
		switch v := f.(type) {
		case *core.UserMessageFrame:
			if v != lastUser && !core.IsSayTurn(v.Turn) { // a spoken greeting is no caller turn
				lastUser = v
				rec.AddTurn("user", s.sidecarText(v.Content), false)
			}
		case *core.AssistantMessageFrame:
			if v != lastBot && v.Content != "" {
				lastBot = v
				rec.AddTurn("assistant", s.sidecarText(v.Content), v.Partial)
			}
		}
	}
	return func() {
		conn.SetAudioTap(nil)
		if err := rec.Close(); err != nil {
			s.logger.Warn("call recording not saved", slog.Uint64("session", sessionID), slog.String("err", err.Error()))
		}
		if d := rec.Dropped(); d > 0 {
			s.logger.Warn("call recording dropped frames", slog.Uint64("session", sessionID), slog.Uint64("frames", d))
		}
	}
}

// sidecarText redacts a turn for the sidecar. This runs on the session loop,
// so it only redacts what the sidecar keeps (4 KiB per turn) plus a margin so
// a number straddling the cut is still seen whole.
func (s *Server) sidecarText(t string) string {
	if len(t) > 4096+256 {
		t = t[:4096+256]
	}
	return s.redactor.Redact(t)
}
