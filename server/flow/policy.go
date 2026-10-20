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
	MinUserTimeout() time.Duration // 0: no floor
}

// reasonIdleTooShort is the one policy problem that also takes a stored flow offline: a call could not even
// be built with that wait. A sound the server no longer offers (or no longer lets flows choose) does not:
// the call plays the server's default instead, so publish refuses it but readiness stays green.
const reasonIdleTooShort = "idle_timeout_too_short"

// policyProblem is one thing a flow asks for that the server does not allow. reason is the readiness
// code, empty when the problem only blocks a publish.
type policyProblem struct{ reason, message string }

// policyProblems checks the start step's call-level choices against the server. Machine reasons feed
// readiness; messages are what a publish says.
func (g Definition) policyProblems(p Policy) []policyProblem {
	c := g.StartStep().Config
	var out []policyProblem
	if b := c.Audio; b != nil && b.Background != nil && b.Background.Name != "" {
		name := b.Background.Name
		switch {
		case !p.AllowFlowBackground():
			out = append(out, policyProblem{"", "audio.background: this server does not let flows choose a background sound (audio.allow_flow_background)"})
		case !slices.Contains(p.Backgrounds(), name):
			avail := "this server has no sounds"
			if names := p.Backgrounds(); len(names) > 0 {
				avail = "available: " + strings.Join(names, ", ")
			}
			out = append(out, policyProblem{"", fmt.Sprintf("audio.background.name %q is not a sound on this server (%s)", name, avail)})
		}
	}
	if rc := g.RouteCue; rc != nil && rc.Name != "" && !slices.Contains(p.Cues(), rc.Name) {
		avail := "this server has no cues"
		if names := p.Cues(); len(names) > 0 {
			avail = "available: " + strings.Join(names, ", ")
		}
		out = append(out, policyProblem{"", fmt.Sprintf("route_cue.name %q is not a sound on this server (%s)", rc.Name, avail)})
	}
	if i := c.Idle; i != nil && i.UserTimeoutMS != nil && *i.UserTimeoutMS > 0 {
		if floor := p.MinUserTimeout(); time.Duration(*i.UserTimeoutMS)*time.Millisecond <= floor {
			out = append(out, policyProblem{reasonIdleTooShort, fmt.Sprintf("idle.user_timeout_ms must exceed %v: a key press is not silence (dtmf.aggregate_timeout)", floor)})
		}
	}
	return out
}

func policyOf(av Availability) (Policy, bool) { p, ok := av.(Policy); return p, ok }
