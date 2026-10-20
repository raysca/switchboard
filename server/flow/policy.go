package flow

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Policy is what the server knows about call behavior that a flow may only choose within: the named
// background sounds, the route sounds, whether flows may choose one, and the shortest quiet-caller wait that is not
// confused with a key press. An Availability that also implements Policy turns these on; without it
// (unit tests) a flow's choices are not checked against a server.
type Policy interface {
	Backgrounds() []string // sorted names of audio.backgrounds
	Cues() []string        // sorted names of audio.cues (route sounds)
	AllowFlowBackground() bool
	MinUserTimeout() time.Duration  // 0: no floor
	TurnManager() bool              // flows' turn settings need the turn manager
	MaxMute() time.Duration         // the server's effective turn.max_mute: a flow can only shorten it
	MaxCallDuration() time.Duration // the server's limits.max_call_duration: a flow can only shorten it
	SmartTurn() bool                // the Smart Turn model is loaded: a flow may ask for end_of_turn "smart"
}

// reasonIdleTooShort is the one policy problem that also takes a stored flow offline: a call could not even
// be built with that wait. A sound the server no longer offers (or no longer lets flows choose) does not:
// the call plays the server's default instead, so publish refuses it but readiness stays green.
const reasonIdleTooShort = "idle_timeout_too_short"

// reasonSmartTurnUnavailable also takes a stored flow offline: a call asking for the Smart Turn model
// could not be built without it.
const reasonSmartTurnUnavailable = "smart_turn_unavailable"

// policyProblem is one thing a flow asks for that the server does not allow. reason is the readiness
// code, empty when the problem only blocks a publish.
type policyProblem struct {
	reason, message string
	storedOK        bool // a copy of a stored version is not refused for it (the call just runs without)
}

// policyProblems checks the start step's call-level choices against the server. Machine reasons feed
// readiness; messages are what a publish says.
func (g Definition) policyProblems(p Policy) []policyProblem {
	c := g.StartStep().Config
	var out []policyProblem
	for _, n := range g.Steps {
		prefix := ""
		if n.ID != g.Start {
			prefix = fmt.Sprintf("step %q: ", n.ID)
		}
		if a := n.Config.Audio; a != nil {
			if msg := backgroundProblem(a.Background, p); msg != "" {
				out = append(out, policyProblem{message: prefix + msg})
			}
		}
	}
	if rc := g.RouteCue; rc != nil && rc.Name != "" && !slices.Contains(p.Cues(), rc.Name) {
		out = append(out, policyProblem{message: unknownCue("route_cue.name", rc.Name, p), storedOK: true})
	}
	if tc := c.Audio; tc != nil && tc.ToolCue != nil && !slices.Contains(p.Cues(), tc.ToolCue.Name) {
		out = append(out, policyProblem{message: unknownCue("audio.tool_cue.name", tc.ToolCue.Name, p), storedOK: true})
	}
	if t := c.Turn; t != nil {
		if !p.TurnManager() {
			out = append(out, policyProblem{message: "turn settings need turn.manager: true on this server"})
		}
		if m := t.MaxMuteMS; m != nil && time.Duration(*m)*time.Millisecond > p.MaxMute() {
			out = append(out, policyProblem{message: fmt.Sprintf("turn.max_mute_ms %d is above this server's limit of %d (turn.max_mute): a flow can only shorten it", *m, p.MaxMute().Milliseconds()), storedOK: true})
		}
		if t.EndOfTurn == EndOfTurnSmart && !p.SmartTurn() {
			out = append(out, policyProblem{reason: reasonSmartTurnUnavailable, message: "Smart end-of-turn needs the Smart Turn model on this server (turn.analyzer.for_flows)"})
		}
	}
	if s := c.MaxCallDurationSeconds; s != 0 && time.Duration(s)*time.Second > p.MaxCallDuration() {
		out = append(out, policyProblem{message: fmt.Sprintf("max_call_duration_seconds %d is above this server's limit of %d (limits.max_call_duration): a flow can only shorten it", s, int(p.MaxCallDuration().Seconds())), storedOK: true})
	}
	if i := c.Idle; i != nil && i.UserTimeoutMS != nil && *i.UserTimeoutMS > 0 {
		if floor := p.MinUserTimeout(); time.Duration(*i.UserTimeoutMS)*time.Millisecond <= floor {
			out = append(out, policyProblem{reason: reasonIdleTooShort, message: fmt.Sprintf("idle.user_timeout_ms must exceed %v: a key press is not silence (dtmf.aggregate_timeout)", floor)})
		}
	}
	return out
}

// unknownCue is the publish message for a cue name the server does not have, listing the ones it has.
func unknownCue(field, name string, p Policy) string {
	avail := "this server has no cues"
	if names := p.Cues(); len(names) > 0 {
		avail = "available: " + strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s %q is not a sound on this server (%s)", field, name, avail)
}

// backgroundProblem says why the server cannot play a step's background sound, or "" when it can.
func backgroundProblem(b *BackgroundSettings, p Policy) string {
	if b == nil || b.Name == "" {
		return ""
	}
	if !p.AllowFlowBackground() {
		return "audio.background: this server does not let flows choose a background sound (audio.allow_flow_background)"
	}
	if names := p.Backgrounds(); !slices.Contains(names, b.Name) {
		avail := "this server has no sounds"
		if len(names) > 0 {
			avail = "available: " + strings.Join(names, ", ")
		}
		return fmt.Sprintf("audio.background.name %q is not a sound on this server (%s)", b.Name, avail)
	}
	return ""
}

func policyOf(av Availability) (Policy, bool) { p, ok := av.(Policy); return p, ok }
