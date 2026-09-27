// Package loadgen produces real-time 16 kHz audio streams made of recorded
// speech clips inserted into low-level noise at known onset timestamps.
package loadgen

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"govoice/voice"
)

const (
	Chunk      = 512
	ChunkDur   = 32 * time.Millisecond
	SampleRate = 16000
)

type Clip struct {
	Samples []float32 // padded to a multiple of Chunk
	Onset   int       // first speech sample
}

type Library struct {
	Clips []Clip
	Noise []float32 // shared silence floor, multiple of Chunk
}

// Load reads 16-bit mono 16 kHz WAVs from dir.
func Load(dir string, noiseRMS float64) (*Library, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.wav"))
	sort.Strings(files)
	lib := &Library{}
	for _, fn := range files {
		pcm, err := readWav(fn)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fn, err)
		}
		c := Clip{Onset: -1}
		for i, v := range pcm {
			if c.Onset < 0 && math.Abs(float64(v)) > 0.02 {
				c.Onset = i
			}
		}
		n := (len(pcm) + Chunk - 1) / Chunk * Chunk
		c.Samples = make([]float32, n)
		copy(c.Samples, pcm)
		lib.Clips = append(lib.Clips, c)
	}
	if len(lib.Clips) == 0 {
		return nil, fmt.Errorf("no clips in %s", dir)
	}
	r := rand.New(rand.NewPCG(1, 2))
	lib.Noise = make([]float32, 30*SampleRate/Chunk*Chunk)
	for i := range lib.Noise {
		lib.Noise[i] = float32(r.NormFloat64() * noiseRMS)
	}
	return lib, nil
}

func readWav(fn string) ([]float32, error) {
	b, err := os.ReadFile(fn)
	if err != nil {
		return nil, err
	}
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a wav")
	}
	for p := 12; p+8 <= len(b); {
		id, sz := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		p += 8
		if id == "fmt " {
			if ch, sr, bits := binary.LittleEndian.Uint16(b[p+2:]), binary.LittleEndian.Uint32(b[p+4:]), binary.LittleEndian.Uint16(b[p+14:]); ch != 1 || sr != SampleRate || bits != 16 {
				return nil, fmt.Errorf("need mono 16k s16, got ch=%d sr=%d bits=%d", ch, sr, bits)
			}
		}
		if id == "data" {
			sz = min(sz, len(b)-p)
			out := make([]float32, sz/2)
			for i := range out {
				out[i] = float32(int16(binary.LittleEndian.Uint16(b[p+2*i:]))) / 32768
			}
			return out, nil
		}
		p += sz + sz&1
	}
	return nil, fmt.Errorf("no data chunk")
}

type seg struct {
	start, n int // in chunks
	clip     int // -1 = noise
	noiseOff int // in chunks
}

// Timeline is a deterministic per-session audio schedule. Chunk(seq) must be
// called with non-decreasing seq (single consumer).
type Timeline struct {
	lib    *Library
	r      *rand.Rand
	duty   float64
	segs   []seg
	end    int // chunks generated so far
	cur    int
	Onsets []int // absolute sample index of each speech onset
	speech int   // chunks of speech generated
}

const minGapChunks = 38 // ~1.2 s: longer than the detector's 800 ms stop window

func NewTimeline(lib *Library, seed uint64, duty float64) *Timeline {
	t := &Timeline{lib: lib, r: rand.New(rand.NewPCG(seed, seed^0x9e3779b9)), duty: duty}
	t.addNoise(16 + t.r.IntN(48)) // 0.5–2 s lead-in so onsets are never at t=0
	return t
}

func (t *Timeline) addNoise(n int) {
	off := t.r.IntN(len(t.lib.Noise)/Chunk - 1)
	t.segs = append(t.segs, seg{t.end, n, -1, off})
	t.end += n
}

func (t *Timeline) extend() {
	if t.duty <= 0 {
		t.addNoise(1000)
		return
	}
	ci := t.r.IntN(len(t.lib.Clips))
	c := t.lib.Clips[ci]
	n := len(c.Samples) / Chunk
	t.Onsets = append(t.Onsets, t.end*Chunk+c.Onset)
	t.segs = append(t.segs, seg{t.end, n, ci, 0})
	t.end += n
	t.speech += n
	gap := int(float64(n) * (1 - t.duty) / t.duty * (0.5 + t.r.Float64()))
	t.addNoise(max(gap, minGapChunks))
}

func (t *Timeline) Chunk(seq uint64) []float32 {
	i := int(seq)
	for i >= t.end {
		t.extend()
	}
	for t.segs[t.cur].start+t.segs[t.cur].n <= i {
		t.cur++
	}
	s := t.segs[t.cur]
	k := i - s.start
	if s.clip < 0 {
		k = (s.noiseOff + k) % (len(t.lib.Noise) / Chunk)
		return t.lib.Noise[k*Chunk : (k+1)*Chunk]
	}
	return t.lib.Clips[s.clip].Samples[k*Chunk : (k+1)*Chunk]
}

// ActualDuty is the fraction of generated chunks that are speech clips.
func (t *Timeline) ActualDuty() float64 {
	if t.end == 0 {
		return 0
	}
	return float64(t.speech) / float64(t.end)
}

// Stream binds a session to a timeline on a real-time clock.
type Stream struct {
	S     *voice.Session
	TL    *Timeline
	Start time.Time
	next  uint64
	Mu    sync.Mutex // guards Detections, TTSFrames and TL (extended by the feeder)

	// Written by the session run loop (OnOutput), read after the session is done.
	Detections []time.Time
	TTSFrames  int
}

// SkipTo makes the stream join its timeline at seq (a call already in progress).
func (st *Stream) SkipTo(seq uint64) { st.next = seq }

func (st *Stream) Due(seq uint64) time.Time { return st.Start.Add(time.Duration(seq+1) * ChunkDur) }

// OnsetTime converts an onset sample index to wall time.
func (st *Stream) OnsetTime(sample int) time.Time {
	return st.Start.Add(time.Duration(sample) * time.Second / SampleRate)
}

// Feeder pushes due chunks for many streams from one goroutine. Sends are
// non-blocking, so a stalled session can never delay the others.
type Feeder struct {
	mu      sync.Mutex
	streams []*Stream
	Lag     voice.Hist // actual send - due
	Sent    atomic.Int64
	Refused atomic.Int64
}

func (f *Feeder) Add(st *Stream) {
	f.mu.Lock()
	f.streams = append(f.streams, st)
	f.mu.Unlock()
}

func (f *Feeder) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

func (f *Feeder) Run(stop <-chan struct{}) {
	t := time.NewTicker(2 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		now := time.Now()
		f.mu.Lock()
		live := f.streams[:0]
		for _, st := range f.streams {
			select {
			case <-st.S.Done():
				continue // drop finished sessions
			default:
			}
			live = append(live, st)
			for due := st.Due(st.next); !due.After(now); due = st.Due(st.next) {
				st.Mu.Lock()
				a := &voice.AudioFrame{Samples: st.TL.Chunk(st.next), Seq: st.next, Due: due}
				st.Mu.Unlock()
				st.next++
				if st.S.PushAudio(a) != nil {
					f.Refused.Add(1)
				} else {
					f.Sent.Add(1)
				}
				f.Lag.Record(time.Since(due))
			}
		}
		clear(f.streams[len(live):])
		f.streams = live
		f.mu.Unlock()
	}
}

// Pool spreads streams over n feeders.
type Pool struct {
	F    []*Feeder
	stop chan struct{}
	i    atomic.Int64
}

func NewPool(n int) *Pool {
	p := &Pool{stop: make(chan struct{})}
	for i := 0; i < n; i++ {
		f := &Feeder{}
		p.F = append(p.F, f)
		go f.Run(p.stop)
	}
	return p
}

func (p *Pool) Add(st *Stream) { p.F[int(p.i.Add(1))%len(p.F)].Add(st) }
func (p *Pool) Stop()          { close(p.stop) }

func (p *Pool) Lag() voice.Snap {
	var s voice.Snap
	for _, f := range p.F {
		x := f.Lag.Snapshot()
		s.Add(&x)
	}
	return s
}

// Score matches detections to known onsets. An onset counts if it is due
// before `until`-600ms. Hit window: [onset-100ms, onset+600ms].
type Score struct {
	Onsets, Hits, Misses, False int
	Delays                      []time.Duration
}

func (st *Stream) Score(from, until time.Time) Score {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	var sc Score
	used := make([]bool, len(st.Detections))
	for _, o := range st.TL.Onsets {
		ot := st.OnsetTime(o)
		if ot.Before(from) || ot.After(until.Add(-600*time.Millisecond)) {
			continue
		}
		sc.Onsets++
		hit := false
		for i, d := range st.Detections {
			if !used[i] && !d.Before(ot.Add(-100*time.Millisecond)) && !d.After(ot.Add(600*time.Millisecond)) {
				used[i], hit = true, true
				sc.Delays = append(sc.Delays, d.Sub(ot))
				break
			}
		}
		if hit {
			sc.Hits++
		} else {
			sc.Misses++
		}
	}
	for i, d := range st.Detections {
		if !used[i] && !d.Before(from) && d.Before(until.Add(-600*time.Millisecond)) {
			sc.False++
		}
	}
	return sc
}
