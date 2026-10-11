package server

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"govoice/core"
)

const (
	maxNotesPerRequest = 8
	maxNoteBytes       = 4096
	// notePrefix marks operator text in the history. It is added here, never taken from the
	// request, so an operator cannot pass a note off as the caller's words.
	notePrefix = "[note from the operator] "

	noteBurst        = 4
	noteRefillPerSec = 1.0
	contextNoteName  = "context_note"
	runLLMName       = "run_llm"
)

// ContextInjector steers one live call from outside its pipeline. Both methods return at once;
// the work happens when the call is idle.
type ContextInjector interface {
	// Note adds an operator note to the history; run also starts a model turn.
	Note(content string, run bool)
	// Run starts a model turn from the current history.
	Run()
	// Ready reports that the call's pipeline is running and can take work.
	Ready() bool
}

type callInjector struct {
	trigger *core.AsyncTrigger
	history *core.ConversationContext
}

func newCallInjector(trigger *core.AsyncTrigger, history *core.ConversationContext) ContextInjector {
	return &callInjector{trigger: trigger, history: history}
}

func (c *callInjector) Ready() bool { return c.trigger.Ready() }

func (c *callInjector) Note(content string, run bool) {
	msg := core.Message{Role: core.RoleUser, Content: notePrefix + content}
	c.trigger.Deliver(core.AsyncToolResult{
		Call:   core.ToolCall{Name: contextNoteName},
		NoTurn: !run,
		Inject: func() error { return c.history.AddMessage(msg) },
	})
}

func (c *callInjector) Run() {
	c.trigger.Deliver(core.AsyncToolResult{Call: core.ToolCall{Name: runLLMName}, Inject: func() error { return nil }})
}

// serverMarkers are the phrases (letters only) that introduce text the server itself places in a
// call's history as a user-role message: operator notes and the idle / turn-completion notices.
var serverMarkers = []string{"notefromtheoperator", "turncompletionnotice", "idlenotice", "systemnotice"}

// bracketRepl turns every bracket form into parentheses.
var bracketRepl = strings.NewReplacer("[", "(", "]", ")", "［", "(", "］", ")", "【", "(", "】", ")", "〔", "(", "〕", ")", "「", "(", "」", ")")

// letters keeps only the letters of s, lower-cased, with full-width Latin folded to ASCII, so
// spacing, punctuation, zero-width characters and full-width spelling do not hide a phrase.
func letters(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0xFF21 && r <= 0xFF3A || r >= 0xFF41 && r <= 0xFF5A { // full-width Latin
			r -= 0xFEE0
		}
		if unicode.IsLetter(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// neutralizeNoteMarker defuses text a caller supplied (spoken or typed) that tries to pass as a
// server-authored notice: when it mentions one of serverMarkers, its brackets become parentheses.
// This is a heuristic (look-alike letters from other scripts are not folded); the model still
// treats caller text as untrusted.
func neutralizeNoteMarker(s string) string {
	l := letters(s)
	for _, m := range serverMarkers {
		if strings.Contains(l, m) {
			return bracketRepl.Replace(s)
		}
	}
	return s
}

// sessionInjector ends with its session: once the session is closing the call is gone, even
// though the live map entry is only removed when teardown finishes.
type sessionInjector struct {
	ContextInjector
	sess *core.Session
}

func (s *sessionInjector) Ended() bool { return s.sess.State() > core.StateRunning }
func (s *sessionInjector) Ready() bool { return !s.Ended() && s.ContextInjector.Ready() }

type liveStatus int

const (
	liveOK liveStatus = iota
	liveMissing
	liveLimited
)

type liveCall struct {
	inj    ContextInjector
	tokens float64
	last   time.Time
}

// liveCalls maps a stored session id to the injector of the call running on this instance.
type liveCalls struct {
	mu  sync.Mutex
	m   map[string]*liveCall
	now func() time.Time
}

func newLiveCalls() *liveCalls { return &liveCalls{m: map[string]*liveCall{}, now: time.Now} }

func (l *liveCalls) Put(id string, inj ContextInjector) {
	l.mu.Lock()
	l.m[id] = &liveCall{inj: inj, tokens: noteBurst, last: l.now()}
	l.mu.Unlock()
}

func (l *liveCalls) Delete(id string) {
	l.mu.Lock()
	delete(l.m, id)
	l.mu.Unlock()
}

// Take returns the call's injector and spends one rate-limit token.
func (l *liveCalls) Take(id string) (ContextInjector, liveStatus) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.m[id]
	if !ok {
		return nil, liveMissing
	}
	now := l.now()
	c.tokens = min(noteBurst, c.tokens+now.Sub(c.last).Seconds()*noteRefillPerSec)
	c.last = now
	if c.tokens < 1 {
		return nil, liveLimited
	}
	c.tokens--
	return c.inj, liveOK
}

type contextNote struct {
	Content string `json:"content"`
}

type contextRequest struct {
	Messages []contextNote `json:"messages"`
	Run      bool          `json:"run"`
}

// validate checks the request and trims each note in place.
func (r *contextRequest) validate() error {
	if len(r.Messages) == 0 || len(r.Messages) > maxNotesPerRequest {
		return fmt.Errorf("messages must hold 1 to %d notes", maxNotesPerRequest)
	}
	for i := range r.Messages {
		c := strings.TrimSpace(r.Messages[i].Content)
		switch {
		case c == "" || len(c) > maxNoteBytes:
			return fmt.Errorf("each note must be 1 to %d bytes", maxNoteBytes)
		case strings.ContainsRune(c, 0):
			return errors.New("notes must not contain NUL")
		}
		r.Messages[i].Content = c
	}
	return nil
}
