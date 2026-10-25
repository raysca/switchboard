package eval

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"govoice/server/agent"
	"govoice/server/storage"
	"govoice/transport/wsclient"
)

const (
	// Principal is the caller label of every eval call; the Calls page shows it.
	Principal      = "agent-eval"
	sessionEndWait = 15 * time.Second
	rowsSettleWait = 500 * time.Millisecond // tool rows are written by a per-call goroutine
	tracesWait     = 5 * time.Second        // handoff spans arrive through the async trace queue
	pollEvery      = 200 * time.Millisecond
)

// Opts bounds one call.
type Opts struct {
	TurnTimeout time.Duration // per typed turn: reply plus settle
	Quiet       time.Duration // no agent activity for this long ends a turn
}

// Agent is a stored agent version resolved for one case: the version that will run, and the
// rewritten (text-only) config the call runs as a draft.
type Agent struct {
	ID, Name  string
	VersionID string
	Version   int
	Start     string
	StepNames map[string]string // step id -> name
	Prompts   map[string]string // step id -> system prompt, for the judges
	Draft     json.RawMessage
	Tools     []agent.ToolSelection
}

// CaseJob is the payload of a queued run_case job: the case as it was when the run was enqueued (a later
// edit does not change a queued run), its hash and which repeat this is. The run row names the version.
type CaseJob struct {
	CaseID   string `json:"case_id"`
	CaseHash string `json:"case_hash"`
	Case     Case   `json:"case"`
	Repeat   int    `json:"repeat"`
}

// Target is where an eval call runs: it issues the draft token, names the public listener to dial
// and reads back what the server stored for the call. The CLI reaches a server over HTTP; the server
// itself reaches its own stores.
type Target interface {
	DraftToken(ctx context.Context, a Agent) (string, error)
	PublicAddr() string
	Stored(ctx context.Context, sessionID string) (Stored, error)
}

// Stored is the server's view of one call.
type Stored struct {
	Session storage.SessionRecord
	Turns   []storage.TurnRecord
	Spans   []storage.TraceSpan
	Tools   []storage.ToolCallRecord
}

// Observed is everything one call showed: the stored rows and what the client saw.
type Observed struct {
	SessionID    string
	Stored       Stored
	Transcript   []Line // the client's view, for reports
	ReplyMS      []int  // typed turn -> first bot-llm-text
	ClientErrors int    // RTVI error messages that are not the call's own end
	Dropped      int    // client events past the cap: the call is only partly seen
}

// sentTurn is one typed caller turn.
type sentTurn struct {
	At   time.Time
	Text string
}

// jti reads the token id (the stored session id) from a JWT's payload. The signature is not
// checked: the server issued the token a moment ago.
func jti(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("voice token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("voice token payload: %w", err)
	}
	var p struct {
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.JTI == "" {
		return "", errors.New("voice token has no jti")
	}
	return p.JTI, nil
}

// activity reports whether an event shows the agent working (text, a tool call, a new step's greeting, an error).
func activity(kind string) bool {
	return kind == "audio" || kind == "error" || strings.HasPrefix(kind, "bot-") || strings.HasPrefix(kind, "llm-function-call")
}

// turnState reads the events after mark: when the agent last showed activity, whether it has said
// anything, and whether a reply is still open. A reply is bracketed by bot-llm-started (sent with its
// first text piece, or, for a reply with no text, just before its stop) and bot-llm-stopped, and a slow
// model or tool leaves gaps inside it: it is open from its start to its stop, while a tool call is in
// flight, and after a tool call ends until the reply that follows it (a transfer's greeting included)
// stops. An RTVI error after a tool call ends closes that reply too: a model call that failed sends no
// stop, and the turn must not wait out -turn-timeout for it.
func turnState(ev []wsclient.Event, mark time.Time) (last time.Time, text, open bool) {
	last = mark
	reply, calls, afterCall := false, 0, false
	for _, e := range ev {
		if !e.At.After(mark) || !activity(e.Kind) {
			continue
		}
		last = e.At
		switch e.Kind {
		case "bot-llm-text":
			text = true
		case "bot-llm-started":
			reply = true
		case "bot-llm-stopped":
			reply, afterCall = false, false
		case "llm-function-call-started":
			calls++
		case "llm-function-call-stopped":
			calls = max(calls-1, 0)
			afterCall = true
		case "error":
			if calls == 0 {
				reply, afterCall = false, false
			}
		}
	}
	return last, text, reply || calls > 0 || afterCall
}

// waitSettled returns once the agent has shown no activity for o.Quiet after mark, has no reply
// open (see turnState) and, when needText, has said something since mark. It returns early (nil)
// when the connection ends, e.g. after end_call, and fails at o.TurnTimeout.
func waitSettled(ctx context.Context, cl *wsclient.Client, mark time.Time, needText bool, o Opts) error {
	deadline := time.Now().Add(o.TurnTimeout)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cl.Closed():
			return nil
		case <-tick.C:
		}
		last, text, open := turnState(cl.Snap(), mark)
		if (text || !needText) && !open && time.Since(last) >= o.Quiet {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no settled reply within %s", o.TurnTimeout)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RunCall makes one call: draft token, dial, caller turns, hang-up (unless the agent ended it),
// then the stored rows. A harness problem (timeout, API error) comes back as err together with
// whatever was observed.
func RunCall(ctx context.Context, t Target, a Agent, next NextFunc, o Opts) (Observed, error) {
	tok, err := t.DraftToken(ctx, a)
	if err != nil {
		return Observed{}, err
	}
	id, err := jti(tok)
	if err != nil {
		return Observed{}, err
	}
	cl, err := wsclient.Dial(ctx, t.PublicAddr(), tok, false)
	if err != nil {
		return Observed{}, fmt.Errorf("call %s: %w", id, err)
	}
	defer cl.Close()

	var (
		errs []error
		hist []Line
		sent []sentTurn
	)
	if err := waitSettled(ctx, cl, cl.T0, false, o); err != nil { // the greeting, if any
		errs = append(errs, fmt.Errorf("greeting: %w", err))
	}
turns:
	for turn := 1; len(errs) == 0; turn++ {
		select {
		case <-cl.Closed(): // the agent hung up
			break turns
		default:
		}
		line, done, err := next(ctx, hist)
		if err != nil {
			errs = append(errs, fmt.Errorf("caller: %w", err))
			break
		}
		if done {
			break
		}
		if turn > maxPersonaTurn {
			errs = append(errs, fmt.Errorf("more than %d caller turns", maxPersonaTurn))
			break
		}
		if _, _, open := turnState(cl.Snap(), time.Time{}); open { // the harness gave up on a slow reply: not the agent's fault
			errs = append(errs, fmt.Errorf("turn %d: reply still open when the next line was sent", turn))
			break
		}
		mark := time.Now()
		sent = append(sent, sentTurn{At: mark, Text: line})
		if err := cl.SendText(line); err != nil {
			select {
			case <-cl.Closed():
				break turns
			default:
				errs = append(errs, fmt.Errorf("turn %d: %w", turn, err))
			}
			break
		}
		hist = append(hist, Line{"caller", line})
		if err := waitSettled(ctx, cl, mark, true, o); err != nil {
			errs = append(errs, fmt.Errorf("turn %d: %w", turn, err))
			break
		}
		hist = append(hist, Line{"agent", cl.Texts(mark, "bot-llm-text", false)})
	}
	cl.Close() // the caller hangs up; a no-op when the agent already did

	st, err := fetchStored(ctx, t, id)
	if err != nil {
		errs = append(errs, err)
	}
	ev := cl.Snap()
	obs := Observed{SessionID: id, Stored: st, Dropped: cl.Dropped()}
	obs.Transcript, obs.ReplyMS = clientView(ev, sent)
	obs.ClientErrors = clientErrors(st.Session.EndReason, ev)
	return obs, errors.Join(errs...)
}

// fetchStored waits for the stored session to end, lets the asynchronous rows land, and reads the
// session, its turns and its traces.
func fetchStored(ctx context.Context, t Target, id string) (Stored, error) {
	var st Stored
	var readErr error // the last failed read: why a session that never ended could not be seen
	ended := false
	for end := time.Now().Add(sessionEndWait); time.Now().Before(end) && !ended; {
		if s, err := t.Stored(ctx, id); err != nil {
			readErr = err
		} else {
			st, ended = s, !s.Session.EndedAt.IsZero()
		}
		if !ended {
			if err := sleepCtx(ctx, pollEvery); err != nil {
				return st, errors.Join(err, readErr)
			}
		}
	}
	if !ended {
		return st, errors.Join(fmt.Errorf("session %s did not end within %s", id, sessionEndWait), readErr)
	}
	if err := sleepCtx(ctx, rowsSettleWait); err != nil {
		return st, err
	}
	// Handoff spans go through the asynchronous trace queue and may land after the tool rows: poll
	// until every transfer tool has its span, or give up loudly.
	for end := time.Now().Add(tracesWait); ; {
		s, err := t.Stored(ctx, id)
		if err != nil {
			return st, fmt.Errorf("session %s traces: %w", id, err)
		}
		st = s
		if tracesComplete(st) {
			return st, nil
		}
		if time.Now().After(end) {
			return st, fmt.Errorf("session %s: traces incomplete: a transfer tool has no handoff span after %s", id, tracesWait)
		}
		if err := sleepCtx(ctx, pollEvery); err != nil {
			return st, err
		}
	}
}

// tracesComplete reports whether every transfer_to_* tool call has its handoff span (active or failed).
func tracesComplete(st Stored) bool {
	transfers, spans := 0, 0
	for _, c := range st.Tools {
		if strings.HasPrefix(c.RemoteName, "transfer_to_") {
			transfers++
		}
	}
	for _, sp := range st.Spans {
		if sp.Name == "handoff.active" || sp.Name == "handoff.failed" {
			spans++
		}
	}
	return spans >= transfers
}

// clientView builds the client-side transcript and the typed-turn reply times from the RTVI events.
func clientView(ev []wsclient.Event, sent []sentTurn) (transcript []Line, replyMS []int) {
	// agentText joins the bot text that arrived in [from, to); a new LLM reply starts a new sentence.
	agentText := func(from, to time.Time) string {
		var b strings.Builder
		for _, e := range ev {
			if e.At.Before(from) || (!to.IsZero() && !e.At.Before(to)) {
				continue
			}
			switch e.Kind {
			case "bot-llm-started":
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
			case "bot-llm-text":
				b.WriteString(e.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	add := func(who, text string) {
		if text != "" {
			transcript = append(transcript, Line{who, text})
		}
	}
	if len(sent) > 0 {
		add("agent", agentText(time.Time{}, sent[0].At))
	} else {
		add("agent", agentText(time.Time{}, time.Time{}))
	}
	for i, s := range sent {
		var next time.Time
		if i+1 < len(sent) {
			next = sent[i+1].At
		}
		add("caller", s.Text)
		add("agent", agentText(s.At, next))
		for _, e := range ev {
			if e.Kind == "bot-llm-text" && !e.At.Before(s.At) && (next.IsZero() || e.At.Before(next)) {
				replyMS = append(replyMS, int(e.At.Sub(s.At).Milliseconds()))
				break
			}
		}
	}
	return transcript, replyMS
}

// clientErrors counts the RTVI error messages that are faults.
func clientErrors(endReason string, ev []wsclient.Event) (n int) {
	for i, e := range ev {
		if e.Kind == "error" && !isEndMessage(endReason, e.Text, i == len(ev)-1) {
			n++
		}
	}
	return n
}

// endMessages is what the server sends as the closing RTVI error message when the stored end reason
// is not the same word (server/server.go: idle controller reasons and the call time limit).
var endMessages = map[string]string{"silent_caller": "user_idle", "agent_stuck": "bot_idle", "time_limit": "max_call_duration"}

// isEndMessage reports whether an RTVI error message is the server closing the call with its end
// reason, which is not a fault: the reason itself, or (as the last event before the close) the
// word the server uses for it.
func isEndMessage(endReason, text string, last bool) bool {
	return endReason != "" && (text == endReason || (last && text == endMessages[endReason]))
}
