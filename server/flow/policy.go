package flow

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Policy is what the server knows about call behavior that a flow may only choose within: the named
// background sounds, whether flows may choose one, and the shortest quiet-caller wait that is not
// confused with a key press. An Availability that also implements Policy turns these on; without it
// (unit tests) a flow's choices are not checked against a server.
type Policy interface {
	Backgrounds() []string // sorted names of audio.backgrounds
	AllowFlowBackground() bool
	MinUserTimeout() time.Duration // 0: no floor
}

const (
	reasonMissingBackground = "missing_audio_background"
	reasonBackgroundDenied  = "audio_background_not_allowed"
)

type policyProblem struct{ reason, message string }

// policyProblems checks the start step's call-level choices against the server. Machine reasons feed
// readiness; messages are what a publish says.
func (g Definition) policyProblems(p Policy) []policyProblem {
	c := g.StartStep().Config
	var out []policyProblem
	if b := c.Audio; b != nil && b.Background != nil {
		name := b.Background.Name
		switch {
		case !p.AllowFlowBackground():
			out = append(out, policyProblem{reasonBackgroundDenied, "audio.background: this server does not let flows choose a background sound (audio.allow_flow_background)"})
		case !slices.Contains(p.Backgrounds(), name):
			avail := "this server has no sounds"
			if names := p.Backgrounds(); len(names) > 0 {
				avail = "available: " + strings.Join(names, ", ")
			}
			out = append(out, policyProblem{reasonMissingBackground, fmt.Sprintf("audio.background.name %q is not a sound on this server (%s)", name, avail)})
		}
	}
	if i := c.Idle; i != nil && i.UserTimeoutMS != nil && *i.UserTimeoutMS > 0 {
		if floor := p.MinUserTimeout(); time.Duration(*i.UserTimeoutMS)*time.Millisecond <= floor {
			out = append(out, policyProblem{"idle_timeout_too_short", fmt.Sprintf("idle.user_timeout_ms must exceed %v: a key press is not silence (dtmf.aggregate_timeout)", floor)})
		}
	}
	return out
}

func policyOf(av Availability) (Policy, bool) { p, ok := av.(Policy); return p, ok }
