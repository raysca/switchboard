// Package record writes a call's audio as a stereo WAV (L=user, R=bot) plus a
// JSON sidecar, through a pluggable Sink. It is provider neutral and
// failure-isolated: the taps never block, and a slow or failing sink only
// costs the recording (frames are dropped and counted).
//
// The bot track must be fed from audio the transport actually released
// (paced-writer write completion), never from audio merely generated, so a
// barge-in flush never lands in the recording.
package record

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/transport/resample"
)

// Config bounds a recording. Zero values select the defaults.
type Config struct {
	SampleRate   int           // file rate; default 16000
	MaxDuration  time.Duration // hard cap; default 1h
	MaxBytes     int64         // hard cap on file size; default 256 MiB (max 4 GiB-1KiB)
	QueueLen     int           // tap queue; default 256 frames
	Holdback     time.Duration // lateness tolerated before a track is padded; default 500ms
	FlushEvery   time.Duration // mix/write cadence; default 250ms
	CloseTimeout time.Duration // max wait for the sink in Close; default 5s
	Now          func() time.Time
}

// Meta identifies the call in the sidecar. Ids only: no names or numbers.
type Meta struct {
	ID        string // recording name; must satisfy ValidID
	SessionID string
	FlowID    string
	CallID    string
}

// Sidecar is the JSON stored next to the WAV.
type Sidecar struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	FlowID    string    `json:"flow_id"`
	CallID    string    `json:"call_id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Format    Format    `json:"format"`
	Duration  int64     `json:"duration_ms"`
	Bytes     int64     `json:"bytes"`
	Truncated bool      `json:"truncated,omitempty"` // hit the duration/size cap
	Dropped   uint64    `json:"dropped_frames,omitempty"`
	Turns     []Turn    `json:"turns,omitempty"`
}

type Format struct {
	Container string `json:"container"`
	Rate      int    `json:"sample_rate"`
	Channels  int    `json:"channels"`
	Bits      int    `json:"bits"`
	Layout    string `json:"layout"`
}

// Turn is one transcript entry; OffsetMS is relative to the recording start.
type Turn struct {
	Role        string `json:"role"` // "user" | "assistant"
	Text        string `json:"text"`
	OffsetMS    int64  `json:"offset_ms"`
	Interrupted bool   `json:"interrupted,omitempty"` // assistant cut off by barge-in; Text is what was heard

}

const (
	maxTurns     = 1000
	maxTurnText  = 4 << 10
	maxFileBytes = 4<<30 - 1<<10
	maxPending   = 10 // seconds of not-yet-written audio per track
	snap         = 60 * time.Millisecond
)

type track int

const (
	user track = iota
	bot
)

type item struct {
	t    track
	pcm  []byte
	rate int
	at   time.Time
}

type seg struct {
	start int64 // sample index on the file timeline
	pcm   []byte
}

type trackState struct {
	pos     int64 // end of the last accepted segment
	pending int64 // samples queued in segs
	segs    []seg
	rs      *resample.Resampler
	rsRate  int
}

// Recorder is one call's recording.
type Recorder struct {
	cfg  Config
	sink Sink
	meta Meta
	t0   time.Time
	capS int64 // max samples on the timeline

	q       chan item
	stop    chan struct{}
	done    chan struct{}
	stopped sync.Once
	closed  atomic.Bool
	dead    atomic.Bool // sink failed or cap reached: taps become no-ops

	dropped atomic.Uint64
	bytes   atomic.Int64

	tmu   sync.Mutex
	turns []Turn

	err       error // set by the writer goroutine; read after done
	truncated bool
}

// ErrCloseTimeout means the sink did not finish within Config.CloseTimeout.
var ErrCloseTimeout = errors.New("record: sink did not finish in time")

// New starts a recorder. It never touches the sink on the caller's goroutine.
func New(sink Sink, meta Meta, cfg Config) *Recorder {
	if cfg.SampleRate <= 0 {
		cfg.SampleRate = 16000
	}
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = time.Hour
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 256 << 20
	}
	if cfg.MaxBytes > maxFileBytes {
		cfg.MaxBytes = maxFileBytes
	}
	if cfg.QueueLen <= 0 {
		cfg.QueueLen = 256
	}
	if cfg.Holdback <= 0 {
		cfg.Holdback = 500 * time.Millisecond
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 250 * time.Millisecond
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	capS := int64(cfg.MaxDuration) * int64(cfg.SampleRate) / int64(time.Second)
	if bySize := (cfg.MaxBytes - wavHeaderLen) / 4; bySize < capS {
		capS = bySize
	}
	r := &Recorder{cfg: cfg, sink: sink, meta: meta, t0: cfg.Now(), capS: capS,
		q: make(chan item, cfg.QueueLen), stop: make(chan struct{}), done: make(chan struct{})}
	go r.run()
	return r
}

// User taps inbound caller audio (call from the transport read path).
func (r *Recorder) User(f *core.InputAudioFrame) {
	if f != nil {
		r.add(user, f.Samples, f.SampleRate, f.Channels, f.Encoding)
	}
}

// Bot taps outbound audio after the paced writer released it successfully.
func (r *Recorder) Bot(f *core.OutputAudioFrame) {
	if f != nil {
		r.add(bot, f.Samples, f.SampleRate, f.Channels, f.Encoding)
	}
}

func (r *Recorder) add(t track, pcm []byte, rate, ch int, enc core.AudioEncoding) {
	if r.closed.Load() || r.dead.Load() || len(pcm) == 0 {
		return
	}
	if (enc != "" && enc != core.EncodingPCM16LE) || ch > 1 || len(pcm)%2 != 0 || rate <= 0 {
		r.dropped.Add(1)
		return
	}
	it := item{t: t, pcm: append([]byte(nil), pcm...), rate: rate, at: r.cfg.Now()}
	select {
	case r.q <- it:
	default:
		r.dropped.Add(1)
	}
}

// AddTurn appends a transcript entry (bounded; extra turns are ignored).
func (r *Recorder) AddTurn(role, text string, interrupted bool) {
	if r.closed.Load() {
		return
	}
	if len(text) > maxTurnText {
		text = text[:maxTurnText]
	}
	off := r.cfg.Now().Sub(r.t0).Milliseconds()
	r.tmu.Lock()
	if len(r.turns) < maxTurns {
		r.turns = append(r.turns, Turn{Role: role, Text: text, OffsetMS: off, Interrupted: interrupted})
	}
	r.tmu.Unlock()
}

// Dropped counts frames discarded (queue full, bad format, late, pending cap).
func (r *Recorder) Dropped() uint64 { return r.dropped.Load() }

// Close stops recording and finalizes whatever was captured (a partial call is
// still a valid file). It waits at most CloseTimeout for the sink.
func (r *Recorder) Close() error {
	r.closed.Store(true)
	r.stopped.Do(func() { close(r.stop) })
	select {
	case <-r.done:
		return r.err
	case <-time.After(r.cfg.CloseTimeout):
		return ErrCloseTimeout
	}
}

func (r *Recorder) off(t time.Time) int64 {
	d := t.Sub(r.t0)
	if d < 0 {
		return 0
	}
	return int64(d) * int64(r.cfg.SampleRate) / int64(time.Second)
}

// writer-goroutine state
type writer struct {
	r       *Recorder
	obj     Object
	tr      [2]trackState
	emitted int64
	failed  bool
}

func (r *Recorder) run() {
	defer close(r.done)
	w := &writer{r: r}
	tick := time.NewTicker(r.cfg.FlushEvery)
	defer tick.Stop()
	for {
		select {
		case it := <-r.q:
			w.ingest(it)
		case <-tick.C:
			w.drain(r.off(r.cfg.Now()) - int64(r.cfg.Holdback)*int64(r.cfg.SampleRate)/int64(time.Second))
		case <-r.stop:
			for len(r.q) > 0 {
				w.ingest(<-r.q)
			}
			w.finish()
			return
		}
	}
}

func (w *writer) open() bool {
	if w.obj != nil {
		return true
	}
	if w.failed {
		return false
	}
	obj, err := w.r.sink.Create(w.r.meta.ID)
	if err == nil {
		_, err = obj.Write(wavHeader(w.r.cfg.SampleRate, 0))
		if err != nil {
			_ = obj.Abort()
		}
	}
	if err != nil {
		w.fail(err)
		return false
	}
	w.obj = obj
	return true
}

func (w *writer) fail(err error) {
	w.failed = true
	w.r.err = err
	w.r.dead.Store(true)
	if w.obj != nil {
		_ = w.obj.Abort()
		w.obj = nil
	}
	for i := range w.tr {
		w.tr[i].segs = nil
	}
}

func (w *writer) ingest(it item) {
	if w.failed {
		return
	}
	r := w.r
	ts := &w.tr[it.t]
	pcm := it.pcm
	if it.rate != r.cfg.SampleRate {
		if ts.rs == nil || ts.rsRate != it.rate {
			rs, err := resample.NewResampler(it.rate, r.cfg.SampleRate)
			if err != nil {
				r.dropped.Add(1)
				return
			}
			ts.rs, ts.rsRate = rs, it.rate
		}
		out, err := ts.rs.ResamplePCM16(pcm)
		if err != nil {
			r.dropped.Add(1)
			return
		}
		pcm = out
	}
	n := int64(len(pcm) / 2)
	if n == 0 {
		return
	}
	// A caller frame arrived when it ended; a bot frame was released when it
	// starts playing. Audio landing within snap of the track's end continues
	// it sample-accurately (network jitter and clock skew never insert
	// clicks); only a larger gap becomes silence. Early audio is never
	// overlapped, so a burst after a stall is delayed, not lost.
	start := r.off(it.at)
	if it.t == user {
		start -= n
	}
	if start < ts.pos+int64(snap)*int64(r.cfg.SampleRate)/int64(time.Second) {
		start = ts.pos
	}
	if start < w.emitted { // arrived after its slot was already written
		skip := w.emitted - start
		if skip >= n {
			r.dropped.Add(1)
			return
		}
		pcm, n, start = pcm[skip*2:], n-skip, w.emitted
	}
	if start >= r.capS {
		r.truncated = true
		r.dead.Store(true) // cap reached: stop copying frames
		return
	}
	if start+n > r.capS {
		n = r.capS - start
		pcm = pcm[:n*2]
		r.truncated = true
	}
	if ts.pending+n > maxPending*int64(r.cfg.SampleRate) {
		r.dropped.Add(1)
		return
	}
	ts.segs = append(ts.segs, seg{start, pcm})
	ts.pending += n
	ts.pos = start + n
}

// drain writes the timeline up to upTo samples (clamped to the cap).
func (w *writer) drain(upTo int64) {
	if upTo > w.r.capS {
		upTo = w.r.capS
	}
	chunk := int64(w.r.cfg.SampleRate) // 1s per write
	for !w.failed && w.emitted < upTo {
		n := upTo - w.emitted
		if n > chunk {
			n = chunk
		}
		if !w.open() {
			return
		}
		buf := make([]byte, n*4)
		w.mix(buf, n)
		if _, err := w.obj.Write(buf); err != nil {
			w.fail(err)
			return
		}
		w.r.bytes.Add(n * 4)
		w.emitted += n
	}
}

// mix interleaves [emitted, emitted+n) of both tracks into buf (L=user,R=bot).
func (w *writer) mix(buf []byte, n int64) {
	lo, hi := w.emitted, w.emitted+n
	for t := range w.tr {
		ts := &w.tr[t]
		kept := ts.segs[:0]
		for _, s := range ts.segs {
			end := s.start + int64(len(s.pcm)/2)
			if s.start < hi && end > lo {
				a, b := max(s.start, lo), min(end, hi)
				for i := a; i < b; i++ {
					src := (i - s.start) * 2
					dst := (i-lo)*4 + int64(t)*2
					buf[dst], buf[dst+1] = s.pcm[src], s.pcm[src+1]
				}
				if end > hi { // remainder stays queued
					kept = append(kept, seg{hi, s.pcm[(hi-s.start)*2:]})
					ts.pending -= hi - max(s.start, lo)
					continue
				}
				ts.pending -= end - max(s.start, lo)
				continue
			}
			if end <= lo { // fully behind: nothing left to write
				ts.pending -= end - s.start
				continue
			}
			kept = append(kept, s)
		}
		ts.segs = kept
	}
}

func (w *writer) finish() {
	r := w.r
	end := max(w.tr[user].pos, w.tr[bot].pos)
	w.drain(end)
	if w.failed {
		return
	}
	if !w.open() { // empty call: still produce a valid, empty file
		return
	}
	r.tmu.Lock()
	turns := append([]Turn(nil), r.turns...)
	r.tmu.Unlock()
	data := w.emitted * 4
	sc, _ := json.Marshal(Sidecar{
		ID: r.meta.ID, SessionID: r.meta.SessionID, FlowID: r.meta.FlowID, CallID: r.meta.CallID,
		StartedAt: r.t0.UTC(), EndedAt: r.cfg.Now().UTC(),
		Format:   Format{"wav", r.cfg.SampleRate, 2, 16, "L=user,R=bot"},
		Duration: w.emitted * 1000 / int64(r.cfg.SampleRate), Bytes: data + wavHeaderLen,
		Truncated: r.truncated, Dropped: r.dropped.Load(), Turns: turns,
	})
	if err := w.obj.Finalize(wavHeader(r.cfg.SampleRate, uint32(data)), sc); err != nil {
		r.err = err
	}
	w.obj = nil
}
