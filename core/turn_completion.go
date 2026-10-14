package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LLM turn-completion protocol (port of Pipecat's user_turn_completion_mixin).
// The model is told, via a system-prompt addendum, to open every reply with a
// marker: complete (reply follows), incomplete-short (cut off mid-thought) or
// incomplete-long (needs time). AssistantContextProcessor reads the marker
// before any text reaches TTS, strips it, and for incomplete turns suppresses
// the generation and arms a wait timer that re-prompts the model afterwards.
// Stages before this one (trace llm.delta text, the llm_first_token timeline
// event) see the raw marker: first-token latency is the model's, and a held
// reply still records one. Downstream (TTS, RTVI, context, recordings) never does.
// A reply that opens with ◐/○ is held even if text follows (Pipecat does the
// same); a ●/◐/○ anywhere later is stripped, never spoken.
//
// The marker glyphs and their default wording follow Pipecat (BSD-2, Daily).

const (
	CompleteMarker        = "●"
	IncompleteShortMarker = "◐"
	IncompleteLongMarker  = "○"
)

// Verdict names, used for counters and debug events.
const (
	VerdictComplete        = "complete"
	VerdictIncompleteShort = "incomplete_short"
	VerdictIncompleteLong  = "incomplete_long"
	VerdictNoMarker        = "no_marker"
)

// Debug events (attributes: status = verdict, duration_ms = wait).
const (
	DebugTurnCompletion      = "turn.completion"       // status: verdict
	DebugTurnCompletionNudge = "turn.completion.nudge" // status: short|long, duration_ms
)

// CompletionInstructions is the default system-prompt addendum.
const CompletionInstructions = `
TURN COMPLETION PROTOCOL (mandatory):
The user's words reach you from speech recognition, usually without punctuation, and sometimes before they have finished talking. Before you reply, decide whether their turn is complete, and start every response with exactly one of these markers as its very first character:

● the user's turn is complete: answer them. Write ●, a space, then your full reply. Never write ● on its own.
◐ the user stopped mid-sentence and will continue in a few seconds. Write ◐ and nothing else.
○ the user needs time to think or asked you to wait. Write ○ and nothing else.

Deciding:
- Complete means conversationally complete, not long. One word can be a complete answer to your question: "yes", "no thanks", "Tuesday". A question to you is complete. A correction, or a request to repeat yourself, is complete.
- If the user has only acknowledged your question or reacted to what you said ("that's a good question", "hmm", "well"), they have not taken their turn yet: ○.
- Cut off (◐): the last words leave a phrase open, in whatever language the user speaks: a sentence that ends on a conjunction, a preposition, an article, or the word for "because"; a list that is still going; a number only partly said. Do not answer it and do not guess the rest: wait with ◐.
- Needs time (○): "hold on", "let me think", "give me a second". Filler followed by a real answer, such as "hmm, I'd go to Japan for the food", is complete.
- When several user messages in a row have no reply from you, your earlier responses were ◐ or ○: judge those fragments together as one turn. If the combined turn still ends open, ◐; if it now answers your question, ●.
- If a tool call is the right response, make the tool call; the turn is complete.

Format rules:
- The marker is the first character. No text, quotes, backticks or explanation before it.
- After ◐ or ○, output nothing: no words, no explanation. The system waits and prompts you again later.
- Exactly one marker per response.

Examples:
- The user says "i'd go to japan because i love". Respond with only ◐.
- The user says "hold on a second". Respond with only ○.
- The user says "japan". Respond with ● followed by your reply, for example "● Japan is a wonderful choice. What draws you there?"
- The user says "can you help me book a flight to new york next week". Respond with "● Of course. What day would you like to leave?"`

// Default re-prompts sent as a synthetic user notice when a wait expires.
const (
	DefaultNudgeShort = `The user paused briefly. Generate a brief, natural prompt (one short sentence) to encourage them to continue. You MUST respond with ● followed by your message; do NOT output ◐ or ○.`
	DefaultNudgeLong  = `The user has been quiet for a while. Generate a brief, friendly check-in (one sentence) that offers to help when they are ready. You MUST respond with ● followed by your message; do NOT output ◐ or ○.`
)

// TurnCompletionConfig enables the protocol on an AssistantContextProcessor.
type TurnCompletionConfig struct {
	ShortWait, LongWait time.Duration // defaults 5s and 10s (Pipecat)
	// Nudge overrides both re-prompts.
	Nudge string
	// Clock nil = wall clock (tests inject the idle fake clock).
	Clock IdleClock
	// OnVerdict mirrors the Verdict* counters (metrics). May be nil.
	OnVerdict func(verdict string)
}

func (c *TurnCompletionConfig) defaults() {
	if c.ShortWait <= 0 {
		c.ShortWait = 5 * time.Second
	}
	if c.LongWait <= 0 {
		c.LongWait = 10 * time.Second
	}
	if c.Clock == nil {
		c.Clock = realIdleClock{}
	}
}

type markerKind int

const (
	markNone markerKind = iota
	markComplete
	markShort
	markLong
)

// parseMarker looks at the start of s. more is true when s is still a possible
// marker prefix (only whitespace, or an unclosed "[..."), so the caller must
// wait for more text. rest is s after the marker and the spaces following it.
func parseMarker(s string) (kind markerKind, rest string, more bool) {
	t := strings.TrimLeft(s, " \t\r\n")
	if t == "" {
		return markNone, s, true
	}
	r := []rune(t)[0]
	switch r {
	case '●', '✓', '✔':
		kind = markComplete
	case '◐':
		kind = markShort
	case '○':
		kind = markLong
	case '[', '(':
		// ASCII fallback: [complete], [incomplete_short], (long) ...
		closer := "]"
		if r == '(' {
			closer = ")"
		}
		end := strings.Index(t, closer)
		if end < 0 {
			return markNone, s, len(t) < 24
		}
		word := strings.ToLower(strings.NewReplacer(" ", "_", "-", "_").Replace(strings.TrimSpace(t[1:end])))
		switch word {
		case "complete":
			kind = markComplete
		case "incomplete_short", "short":
			kind = markShort
		case "incomplete_long", "long":
			kind = markLong
		default:
			return markNone, s, false
		}
		return kind, strings.TrimLeft(t[end+1:], " "), false
	default:
		return markNone, s, false
	}
	return kind, strings.TrimLeft(t[len(string(r)):], " "), false
}

// completionGate is the per-generation marker state. lenient generations
// (synthetic turns, or a tool call before any text) strip a marker but never
// hold: the turn is complete by construction.
type completionGate struct {
	buf     strings.Builder
	decided bool
	hold    bool
	lenient bool
}

func isSyntheticTurn(turn string) bool {
	return strings.HasPrefix(turn, "idle-") || strings.HasPrefix(turn, "async-") || strings.HasPrefix(turn, sayTurnPrefix) || strings.HasPrefix(turn, "completion-")
}

// gateFeed consumes a delta and returns the text to release (possibly empty).
func (p *AssistantContextProcessor) gateFeed(delta string) string {
	g := p.gate
	if g.decided {
		if g.hold {
			return ""
		}
		return stripMarkers(delta)
	}
	g.buf.WriteString(delta)
	return p.gateDecide(g.buf.String(), false)
}

// gateDecide settles the verdict from the accumulated text; atEnd forces a
// decision on a still-ambiguous prefix. Returns the text to release.
func (p *AssistantContextProcessor) gateDecide(text string, atEnd bool) string {
	g := p.gate
	kind, rest, more := parseMarker(text)
	if more && !atEnd {
		return ""
	}
	g.decided = true
	g.buf.Reset()
	if kind == markNone {
		p.verdict(VerdictNoMarker)
		return stripMarkers(text)
	}
	if g.lenient || kind == markComplete {
		p.verdict(VerdictComplete)
		return stripMarkers(rest)
	}
	g.hold = true
	if kind == markShort {
		p.verdict(VerdictIncompleteShort)
	} else {
		p.verdict(VerdictIncompleteLong)
	}
	p.holdShort = kind == markShort
	return ""
}

func (p *AssistantContextProcessor) verdict(v string) {
	if cb := p.cfg.Completion.OnVerdict; cb != nil {
		cb(v)
	}
	p.debug(p.activeTurn, DebugTurnCompletion, map[string]any{"status": v})
}

// debug takes the turn explicitly: the nudge runs off the processor goroutine
// and must not read activeTurn.
func (p *AssistantContextProcessor) debug(turn, name string, attrs map[string]any) {
	if p.session == nil {
		return
	}
	if d, ok := p.session.(interface{ EmitDebug(DebugEvent) }); ok {
		d.EmitDebug(NewDebugEvent(name, time.Now(), SessionID(strconv.FormatUint(p.session.ID(), 10)), TurnID(turn), Generation(p.session.CurrentGeneration()), attrs))
	}
}

// armHold waits for the user to continue; if they stay silent the model is
// re-prompted with a synthetic user notice. Any user activity cancels it.
func (p *AssistantContextProcessor) armHold(short bool) {
	p.cancelHold()
	if p.session == nil || p.index == 0 {
		return
	}
	c := p.cfg.Completion
	wait, text, kind := c.LongWait, DefaultNudgeLong, "long"
	if short {
		wait, text, kind = c.ShortWait, DefaultNudgeShort, "short"
	}
	if c.Nudge != "" {
		text = c.Nudge
	}
	stop := make(chan struct{})
	p.holdMu.Lock()
	p.holdStop = stop
	p.held.Store(true)
	p.holdMu.Unlock()
	timer := c.Clock.TimerAt(c.Clock.Now().Add(wait))
	if p.session.Go(func(ctx context.Context) {
		defer timer.Stop()
		select {
		case <-timer.C():
		case <-stop:
			return
		case <-ctx.Done():
			return
		}
		p.holdMu.Lock()
		if p.holdStop != stop {
			p.holdMu.Unlock()
			return
		}
		p.holdStop = nil
		p.held.Store(false)
		p.holdMu.Unlock()
		p.nudge(kind, text, wait)
	}) != nil {
		p.cancelHold()
	}
}

// cancelHold drops a pending wait (safe from any goroutine).
func (p *AssistantContextProcessor) cancelHold() {
	p.holdMu.Lock()
	if p.holdStop != nil {
		close(p.holdStop)
		p.holdStop = nil
	}
	p.held.Store(false)
	p.holdMu.Unlock()
}

// Held reports an incomplete user turn is waiting for its continuation (the
// idle controller must not prompt meanwhile).
func (p *AssistantContextProcessor) Held() bool { return p.held.Load() }

func (p *AssistantContextProcessor) nudge(kind, text string, wait time.Duration) {
	n := p.nudgeN.Add(1)
	turn := fmt.Sprintf("completion-%d", n)
	at := time.Now()
	content := "[turn-completion notice; system notice, not spoken by the user] " + text
	if p.ctxPool.AddMessage(Message{Role: RoleUser, TurnID: turn, CreatedAt: at, Content: content, Metadata: map[string]any{"completion_nudge": kind}}) != nil {
		return
	}
	p.debug(turn, DebugTurnCompletionNudge, map[string]any{"status": kind, "duration_ms": wait.Milliseconds()})
	// Resume at the LLM stage, which sits directly before this one.
	_ = p.session.Reenter(&UserMessageFrame{Turn: turn, Content: content, Gen: p.session.CurrentGeneration(), Timestamp: at}, Downstream, p.index-1, 0)
}

// gateFinal settles the verdict on the complete response text and returns the
// text to record (marker stripped), or held for an incomplete turn.
func (p *AssistantContextProcessor) gateFinal(text string) (out string, held bool) {
	g := p.gate
	if !g.decided {
		out = p.gateDecide(text, true)
		return out, g.hold
	}
	if g.hold {
		return "", true
	}
	if k, rest, _ := parseMarker(text); k != markNone {
		return stripMarkers(rest), false
	}
	return stripMarkers(text), false
}

// stripMarkers drops marker glyphs anywhere in released text: a model repeats
// the marker at the start of each response round (after a tool result, or a
// failover restart) and TTS must never read a glyph aloud. Rune-wise, so a
// delta stream and its final text strip identically.
var stripMarkers = strings.NewReplacer(CompleteMarker, "", IncompleteShortMarker, "", IncompleteLongMarker, "").Replace
