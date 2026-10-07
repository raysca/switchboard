package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

// DebugSTTFailover is emitted when the active STT candidate changes. Attributes
// (the trace allowlist): component (switched from), provider (switched to) and
// reason (failover class, or "recovered" when traffic returns to a better
// candidate at a turn boundary); all bounded.
const DebugSTTFailover = "stt.failover"

const (
	// DefaultSTTReplayBytes is the default replay buffer: 10 s of 16 kHz PCM16 mono.
	DefaultSTTReplayBytes = 10 * 16000 * 2
	// DefaultSTTPreRollBytes is how much audio before the VAD speech-start signal
	// belongs to the utterance (VAD reports onset late): 1 s of 16 kHz PCM16.
	DefaultSTTPreRollBytes = 16000 * 2
	maxJournalEntries      = 4096
)

// STTCounters is implemented by STT processors that keep stream counters (and
// the failover wrapper, which sums its candidates) so the server exports them.
type STTCounters interface {
	STTCounts() (rotations, rotationFailures, deduplicated, errors uint64)
}

// STTCounts sums the counters of every candidate that reports them.
func (w *STTFailover) STTCounts() (rot, rotFail, dedup, errs uint64) {
	for _, c := range w.cfg.Candidates {
		if m, ok := c.Proc.(STTCounters); ok {
			a, b, d, e := m.STTCounts()
			rot, rotFail, dedup, errs = rot+a, rotFail+b, dedup+d, errs+e
		}
	}
	return
}

// STTCandidate is one STT processor of a failover chain.
type STTCandidate struct {
	Provider string
	Proc     Processor
	// Open reports whether this candidate's circuit breaker would refuse now.
	// Open candidates are tried last. May be nil.
	Open func() bool
}

type STTFailoverConfig struct {
	Candidates []STTCandidate // ordered, primary first
	// Classify maps a candidate's failure to a failover class; "" never fails over.
	Classify func(error) string
	// Allow reports whether the class may fail over. Nil allows every class.
	Allow func(class string) bool
	// BufferBytes bounds the replay buffer (default DefaultSTTReplayBytes);
	// PreRollBytes is the audio before the speech-start signal replayed with the
	// utterance (default DefaultSTTPreRollBytes).
	BufferBytes, PreRollBytes int
	// OnFailover is called for every switch (metrics). May be nil.
	OnFailover func(from, to, class string)
	// OnDropped is called with the bytes of open-utterance audio evicted from a
	// full buffer (that utterance can no longer be replayed whole). May be nil.
	OnDropped func(bytes int)
}

// STTFailover is an STT stage over an ordered chain of candidates. Audio goes to
// the active candidate and, copied, into a bounded buffer. When the active
// candidate fails with a failover-class error the next one becomes active and
// the buffered audio of the open utterance (with the control frames the failed
// candidate saw) is replayed to it, so the utterance is transcribed whole by the
// replacement, whose cumulative text supersedes the failed candidate's. Between
// utterances nothing is replayed: each utterance start re-orders the chain
// (breaker-open candidates last), so traffic returns to the primary once its
// breaker allows.
//
// Candidates are attached through a proxy session that sees what they re-enter;
// everything an abandoned candidate still emits is dropped, whatever its generation.
type STTFailover struct {
	cfg     STTFailoverConfig
	session SessionController
	index   int

	// sendMu serializes delivery to candidates (live frames vs failover replay).
	// Order: sendMu before mu; candidates are never called with mu held.
	sendMu sync.Mutex

	mu        sync.Mutex
	journal   []journalEntry
	audio     int // audio bytes in journal
	nextSeq   uint64
	dropped   uint64
	order     []int // candidate indexes for this utterance, breaker-open ones last
	pos       int   // position in order of the active candidate
	active    int   // candidate index receiving frames
	abandoned []bool
	demoted   []bool // failed between utterances: last at the next utterance start
	open      bool   // an utterance is in progress or awaiting its final
	stopped   bool   // its UserStoppedSpeaking arrived
	mark      uint64 // journal seq where the utterance replay starts
	truncated bool   // utterance audio fell out of the buffer
	lastFinal string // latest final text emitted for the utterance (after carry)
	carry     string // prefix replacement candidates lack because of truncation
	turn      string
	gen       uint64
}

type journalEntry struct {
	seq uint64
	ctx context.Context // the frame's own (trace parentage for replays)
	f   Frame
	dir Direction
	n   int // audio bytes
}

func NewSTTFailover(cfg STTFailoverConfig) (*STTFailover, error) {
	if len(cfg.Candidates) == 0 {
		return nil, errors.New("core: stt failover needs at least one candidate")
	}
	for _, c := range cfg.Candidates {
		if c.Proc == nil {
			return nil, errors.New("core: stt failover candidate has no processor")
		}
	}
	if cfg.BufferBytes <= 0 {
		cfg.BufferBytes = DefaultSTTReplayBytes
	}
	if cfg.PreRollBytes <= 0 {
		cfg.PreRollBytes = DefaultSTTPreRollBytes
	}
	n := len(cfg.Candidates)
	w := &STTFailover{cfg: cfg, abandoned: make([]bool, n), demoted: make([]bool, n)}
	w.order = w.orderLocked()
	return w, nil
}

// Dropped is the open-utterance audio bytes evicted from the replay buffer so far.
func (w *STTFailover) Dropped() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// Buffered is the audio bytes currently held for replay.
func (w *STTFailover) Buffered() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.audio
}

// Active is the provider currently receiving audio.
func (w *STTFailover) Active() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cfg.Candidates[w.active].Provider
}

type sttProxySession struct {
	SessionController
	w   *STTFailover
	idx int
}

func (p sttProxySession) Reenter(f Frame, dir Direction, resume int, gen uint64) error {
	return p.w.fromCandidate(p.idx, f, dir, resume, gen)
}

func (w *STTFailover) Attach(s SessionController, index int) error {
	w.session, w.index = s, index
	for i, c := range w.cfg.Candidates {
		if a, ok := c.Proc.(Attacher); ok {
			if err := a.Attach(sttProxySession{SessionController: s, w: w, idx: i}, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *STTFailover) Close() error {
	var errs []error
	for _, c := range w.cfg.Candidates {
		if cl, ok := c.Proc.(Closer); ok {
			errs = append(errs, cl.Close())
		}
	}
	return errors.Join(errs...)
}

// orderLocked is the candidate order for a new utterance: primary first, demoted
// and breaker-open ones last (their relative order kept).
func (w *STTFailover) orderLocked() []int {
	var up, down []int
	for i, c := range w.cfg.Candidates {
		if w.demoted[i] || (c.Open != nil && c.Open()) {
			down = append(down, i)
		} else {
			up = append(up, i)
		}
	}
	clear(w.demoted)
	return append(up, down...)
}

func (w *STTFailover) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	// Turn events and interruptions reach STT upstream too (the turn manager's
	// rebind and the assistant's barge-in sit after it): journal them either way.
	switch f.(type) {
	case *TurnEventFrame:
		return w.deliver(ctx, f, 0, dir, e, false)
	case *InterruptionFrame, *CancelFrame:
		return w.deliver(ctx, f, 0, dir, e, true)
	}
	if dir != Downstream {
		return w.passToActive(ctx, f, dir, e)
	}
	switch fr := f.(type) {
	case *InputAudioFrame:
		// ponytail: frames are immutable once posted (producers allocate per frame,
		// filters copy), so the journal keeps the frame itself: no per-frame copy.
		return w.deliver(ctx, f, len(fr.Samples), dir, e, false)
	case *UserStartedSpeakingFrame:
		return w.startUtterance(ctx, fr, dir, e)
	case *UserStoppedSpeakingFrame:
		return w.deliver(ctx, f, 0, dir, e, false)
	}
	return w.passToActive(ctx, f, dir, e)
}

func (w *STTFailover) passToActive(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	w.mu.Lock()
	p := w.cfg.Candidates[w.active].Proc
	w.mu.Unlock()
	return p.ProcessFrame(ctx, f, dir, e)
}

// recordLocked journals jf and updates the utterance state.
// mu held.
func (w *STTFailover) recordLocked(ctx context.Context, jf Frame, n int, dir Direction) {
	w.nextSeq++
	w.journal = append(w.journal, journalEntry{seq: w.nextSeq, ctx: ctx, f: jf, dir: dir, n: n})
	w.audio += n
	for w.audio > w.cfg.BufferBytes || len(w.journal) > maxJournalEntries {
		// Evict the oldest audio; the open utterance's control frames stay, or a
		// replay could not rebind the replacement to the turn.
		i := slices.IndexFunc(w.journal, func(en journalEntry) bool { return en.n > 0 })
		if i < 0 {
			i = 0
		}
		old := w.journal[i]
		w.journal = slices.Delete(w.journal, i, i+1)
		w.audio -= old.n
		// Idle audio ageing out is the buffer working; only the open utterance's
		// audio is a loss (it cannot be replayed whole).
		if old.n > 0 && w.open && old.seq >= w.mark {
			w.truncated = true
			w.dropped += uint64(old.n)
			if w.cfg.OnDropped != nil {
				w.cfg.OnDropped(old.n)
			}
		}
		for len(w.journal) > 0 && w.journal[0].n == 0 && (!w.open || w.journal[0].seq < w.mark) {
			w.journal = w.journal[1:]
		}
	}
	switch fr := jf.(type) {
	case *UserStoppedSpeakingFrame:
		w.stopped = true
	case *TurnEventFrame:
		if fr.Kind == TurnEventCompleted || fr.Kind == TurnEventInterrupted {
			w.open = false
		}
	}
}

// deliver gives a frame to the active candidate; if that fails with a
// failover-class error it moves to the next candidate (replaying the open
// utterance, which includes this frame) and forwards the frame itself, which the
// failed candidate had not pushed.
func (w *STTFailover) deliver(ctx context.Context, f Frame, n int, dir Direction, e Emitter, broadcast bool) error {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	w.mu.Lock()
	w.recordLocked(ctx, f, n, dir)
	idx := w.active
	w.mu.Unlock()
	err := w.cfg.Candidates[idx].Proc.ProcessFrame(ctx, f, dir, e)
	if err != nil {
		return w.syncFailure(idx, err, f, dir, e)
	}
	if broadcast { // the others may still drain a previous turn's stream: cancel it quietly
		w.mu.Lock()
		act := w.active
		w.mu.Unlock()
		for i, c := range w.cfg.Candidates {
			if i != act {
				_ = c.Proc.ProcessFrame(ctx, f, dir, swallow{})
			}
		}
	}
	return nil
}

// syncFailure handles a candidate's ProcessFrame error (stream open or write
// failure). sendMu held.
func (w *STTFailover) syncFailure(idx int, err error, f Frame, dir Direction, e Emitter) error {
	class := w.classOf(err)
	w.mu.Lock()
	ok := w.mayFailOverLocked(idx, class)
	w.mu.Unlock()
	if !ok {
		return err
	}
	if jerr := w.jump(idx, false, class, true); jerr != nil {
		return jerr
	}
	return e.PushDirection(f, dir)
}

func (w *STTFailover) startUtterance(ctx context.Context, fr *UserStartedSpeakingFrame, dir Direction, e Emitter) error {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	w.mu.Lock()
	order := w.orderLocked()
	w.order, w.pos = order, 0
	// Replay starts PreRoll bytes of audio before the speech-start signal.
	w.mark = w.nextSeq + 1
	// It never reaches into the previous utterance (its stop/start frames bound it).
	for i, got := len(w.journal)-1, 0; i >= 0 && got < w.cfg.PreRollBytes; i-- {
		switch w.journal[i].f.(type) {
		case *UserStoppedSpeakingFrame, *UserStartedSpeakingFrame:
			i = -1
			continue
		}
		got += w.journal[i].n
		w.mark = w.journal[i].seq
	}
	w.open, w.stopped, w.truncated, w.lastFinal, w.carry = true, false, false, "", ""
	w.turn, w.gen = fmt.Sprintf("turn-%d-%d", fr.Gen, fr.Seq), fr.Gen
	w.recordLocked(ctx, fr, 0, dir)
	from := w.active
	w.mu.Unlock()
	if order[0] != from {
		// Re-evaluation: a better candidate than the one serving idle audio.
		class := "recovered"
		if c := w.cfg.Candidates[from]; c.Open != nil && c.Open() {
			class = "circuit_open"
		}
		if err := w.jump(from, true, class, true); err != nil {
			return err
		}
		// The one left holds an unbound stream of idle audio: end it, so it cannot
		// hand that stale audio to a later utterance. Off the frame path (it may be dead).
		old := w.cfg.Candidates[from].Proc
		_ = w.session.Go(func(ctx context.Context) {
			_ = old.ProcessFrame(ctx, &UserStoppedSpeakingFrame{}, Downstream, swallow{})
		})
		return e.PushDirection(fr, dir)
	}
	err := w.cfg.Candidates[from].Proc.ProcessFrame(ctx, fr, dir, e)
	if err == nil {
		return nil
	}
	return w.syncFailure(from, err, fr, dir, e)
}

func (w *STTFailover) classOf(err error) string {
	if w.cfg.Classify == nil {
		return ""
	}
	return w.cfg.Classify(err)
}

// mayFailOverLocked: idx is the active candidate, the class allowed and one more
// candidate remains.
func (w *STTFailover) mayFailOverLocked(idx int, class string) bool {
	return idx == w.active && class != "" && (w.cfg.Allow == nil || w.cfg.Allow(class)) && w.pos+1 < len(w.order)
}

// jump makes the next candidate active and replays the open utterance to it; one
// that cannot start counts as failed too. Returns an error when the chain is
// exhausted. from is the active candidate. lazy is the utterance-start
// re-evaluation: pos already points at the target and nothing failed. A call for
// a candidate that is no longer active is a no-op. last replays the frame being
// delivered even when no utterance is open. sendMu held.
func (w *STTFailover) jump(from int, lazy bool, class string, last bool) error {
	for first := true; ; first = false {
		w.mu.Lock()
		if w.active != from {
			w.mu.Unlock()
			return nil // moved by a racing path
		}
		failed := !(lazy && first)
		if failed {
			w.pos++
		}
		if w.pos >= len(w.order) {
			w.abandoned[w.active] = false // nothing left: its errors are the call's
			w.mu.Unlock()
			return errors.New("core: stt failover chain exhausted")
		}
		to := w.order[w.pos]
		if failed {
			w.abandoned[from] = true
			if !w.open {
				w.demoted[from] = true // idle failure: last at the next utterance start
			}
		}
		w.active, w.abandoned[to] = to, false
		if w.truncated {
			w.carry = w.lastFinal
		} else {
			w.carry = ""
		}
		var entries []journalEntry
		switch {
		case w.open:
			for _, en := range w.journal {
				if en.seq >= w.mark {
					entries = append(entries, en)
				}
			}
		case last && len(w.journal) > 0:
			entries = []journalEntry{w.journal[len(w.journal)-1]}
		}
		fromName, toName, turn, gen := w.cfg.Candidates[from].Provider, w.cfg.Candidates[to].Provider, w.turn, w.gen
		w.mu.Unlock()
		w.noteFailover(fromName, toName, class, turn, gen)
		var err error
		for _, en := range entries {
			if err = w.cfg.Candidates[to].Proc.ProcessFrame(en.ctx, en.f, en.dir, swallow{}); err != nil {
				break
			}
		}
		if err == nil {
			return nil
		}
		from, last = to, false
		if class = w.classOf(err); class == "" {
			class = "server_error"
		}
	}
}

// fromCandidate sees every frame a candidate re-enters.
func (w *STTFailover) fromCandidate(idx int, f Frame, dir Direction, resume int, gen uint64) error {
	w.mu.Lock()
	if w.abandoned[idx] {
		w.mu.Unlock() // whatever its generation: the replacement owns the utterance now
		return nil
	}
	if idx != w.active {
		w.mu.Unlock() // a previous turn's stream still draining
		return w.session.Reenter(f, dir, resume, gen)
	}
	var class string
	switch fr := f.(type) {
	case *TranscriptCandidateFrame:
		if w.carry != "" {
			cp := *fr
			cp.Text = mergeOverlap(w.carry, fr.Text)
			f, fr = &cp, &cp
		}
		if fr.Final {
			w.lastFinal = fr.Text
			if w.stopped {
				w.open = false // the replacement delivered; nothing left to replay
			}
		}
	case *ErrorFrame:
		if !fr.Fatal {
			class = w.classOf(fr.Err)
			if errors.Is(fr.Err, ErrTranscriptInvalidated) {
				class = "server_error" // the provider lost the turn: recoverable by replay
			}
		}
	case *TranscriptInvalidationFrame:
		class = "server_error"
	}
	if class == "" || !w.mayFailOverLocked(idx, class) {
		w.mu.Unlock()
		return w.session.Reenter(f, dir, resume, gen)
	}
	w.abandoned[idx] = true // drop its remaining output from now on
	w.mu.Unlock()
	// Replay runs on its own goroutine: it needs sendMu, which a live delivery may
	// hold while this very candidate is inside ProcessFrame.
	if err := w.session.Go(func(context.Context) {
		w.sendMu.Lock()
		defer w.sendMu.Unlock()
		if err := w.jump(idx, false, class, false); err != nil {
			w.mu.Lock()
			w.abandoned[w.active] = false
			w.mu.Unlock()
			_ = w.session.Reenter(f, dir, resume, gen) // chain exhausted: the original failure
		}
	}); err != nil {
		w.mu.Lock()
		w.abandoned[idx] = false
		w.mu.Unlock()
		return w.session.Reenter(f, dir, resume, gen)
	}
	return nil
}

// mergeOverlap joins carry (text the replacement could not hear because its
// audio fell out of the buffer) and text, dropping the words text repeats from
// the end of carry (the replay window may reach back into finalized audio).
// Words compare ignoring case and edge punctuation: providers format differently
// ("Two." vs "two"), and failover usually crosses providers.
// ponytail: word-level longest overlap; a repeated phrase at the seam is ambiguous,
// and scripts without spaces (CJK) never overlap (concatenated).
func mergeOverlap(carry, text string) string {
	c, t := strings.Fields(carry), strings.Fields(text)
	same := func(a, b string) bool { return strings.EqualFold(bareWord(a), bareWord(b)) }
	for k := min(len(c), len(t)); k > 0; k-- {
		if slices.EqualFunc(c[len(c)-k:], t[:k], same) {
			return strings.Join(append(c, t[k:]...), " ")
		}
	}
	return strings.Join(append(c, t...), " ")
}

func bareWord(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

func (w *STTFailover) noteFailover(from, to, class, turn string, gen uint64) {
	if w.cfg.OnFailover != nil {
		w.cfg.OnFailover(from, to, class)
	}
	if em, ok := w.session.(interface{ EmitDebug(DebugEvent) }); ok {
		em.EmitDebug(NewDebugEvent(DebugSTTFailover, time.Now(), "", TurnID(turn), Generation(gen),
			map[string]any{"component": from, "provider": to, "reason": class}))
	}
}
