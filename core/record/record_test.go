package record

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(d time.Duration) {
	c.mu.Lock()
	c.t = time.Unix(1_000_000, 0).Add(d)
	c.mu.Unlock()
}

func newClock() *clock { c := &clock{}; c.Set(0); return c }

// pcm returns ms of constant-amplitude 16 kHz PCM16.
func pcm(ms int, v int16) []byte {
	b := make([]byte, ms*16*2)
	for i := 0; i < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], uint16(v))
	}
	return b
}

func in(ms int, v int16) *core.InputAudioFrame {
	return &core.InputAudioFrame{Samples: pcm(ms, v), Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1}
}
func out(ms int, v int16) *core.OutputAudioFrame {
	return &core.OutputAudioFrame{Samples: pcm(ms, v), Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1}
}

// wav parses a finalized file and validates its header.
func wav(t *testing.T, path string) (rate int, l, r []int16) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[0:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || string(b[36:40]) != "data" {
		t.Fatalf("bad header % x", b[:44])
	}
	data := binary.LittleEndian.Uint32(b[40:])
	if int(data) != len(b)-44 || binary.LittleEndian.Uint32(b[4:]) != 36+data {
		t.Fatalf("sizes: data=%d file=%d riff=%d", data, len(b), binary.LittleEndian.Uint32(b[4:]))
	}
	if binary.LittleEndian.Uint16(b[20:]) != 1 || binary.LittleEndian.Uint16(b[22:]) != 2 || binary.LittleEndian.Uint16(b[34:]) != 16 {
		t.Fatal("not stereo PCM16")
	}
	rate = int(binary.LittleEndian.Uint32(b[24:]))
	for i := 44; i+4 <= len(b); i += 4 {
		l = append(l, int16(binary.LittleEndian.Uint16(b[i:])))
		r = append(r, int16(binary.LittleEndian.Uint16(b[i+2:])))
	}
	return
}

func newRec(t *testing.T, cfg Config) (*Recorder, *clock, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rec")
	s, err := NewLocalSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := newClock()
	cfg.Now = c.Now
	return New(s, Meta{ID: "a1-7-1", SessionID: "7", AgentID: "a1", CallID: "7"}, cfg), c, dir
}

func nonzero(s []int16, from, to int) bool {
	for _, v := range s[from:to] {
		if v == 0 {
			return false
		}
	}
	return true
}
func zero(s []int16, from, to int) bool {
	for _, v := range s[from:to] {
		if v != 0 {
			return false
		}
	}
	return true
}

func TestAlignmentAndSilencePadding(t *testing.T) {
	r, c, dir := newRec(t, Config{})
	c.Set(100 * time.Millisecond)
	r.User(in(100, 1000)) // caller audio 0-100ms
	c.Set(time.Second)
	r.Bot(out(100, -2000)) // bot starts playing at 1.0s
	r.AddTurn("user", "hello", false)
	c.Set(1300 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	rate, l, rt := wav(t, filepath.Join(dir, "a1-7-1.wav"))
	if rate != 16000 || len(l) != 1100*16 {
		t.Fatalf("rate=%d samples=%d", rate, len(l))
	}
	if !nonzero(l, 0, 1600) || !zero(l, 1600, len(l)) {
		t.Fatal("user track misaligned")
	}
	if !zero(rt, 0, 16000) || !nonzero(rt, 16000, 17600) {
		t.Fatal("bot track misaligned")
	}
	var sc Sidecar
	b, _ := os.ReadFile(filepath.Join(dir, "a1-7-1.json"))
	if err := json.Unmarshal(b, &sc); err != nil || sc.SessionID != "7" || sc.AgentID != "a1" || sc.Duration != 1100 || len(sc.Turns) != 1 || sc.Format.Layout != "L=user,R=bot" {
		t.Fatalf("sidecar %+v err=%v", sc, err)
	}
}

func TestContiguousAudioHasNoGapsAndResamples(t *testing.T) {
	r, c, dir := newRec(t, Config{})
	for i := 0; i < 5; i++ { // 5 x 20ms bot frames, 24 kHz, released at real time from 200ms
		c.Set(time.Duration(200+i*20) * time.Millisecond)
		f := out(20, 500)
		f.Samples = make([]byte, 24*20*2)
		for j := 0; j < len(f.Samples); j += 2 {
			binary.LittleEndian.PutUint16(f.Samples[j:], 500)
		}
		f.SampleRate = 24000
		r.Bot(f)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, rt := wav(t, filepath.Join(dir, "a1-7-1.wav"))
	if len(rt) < 3200+1580 || len(rt) > 3200+1620 || !zero(rt, 0, 3200) { // 200ms silence + ~100ms resampled
		t.Fatalf("len=%d", len(rt))
	}
}

// Late arrivals (jitter) and a slow sender clock must not punch silence into
// continuous caller audio; a real pause still does.
func TestJitterDoesNotInsertGaps(t *testing.T) {
	r, c, dir := newRec(t, Config{})
	at := 0
	for i, j := range []int{20, 28, 12, 30, 10, 21, 21, 21, 19, 25} { // ~20ms frames, jittery arrivals
		at += j
		c.Set(time.Duration(at) * time.Millisecond)
		r.User(in(20, int16(100+i)))
	}
	c.Set(time.Duration(at+300) * time.Millisecond) // 300ms pause
	r.User(in(20, 1))
	c.Set(2 * time.Second)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	_, l, _ := wav(t, filepath.Join(dir, "a1-7-1.wav"))
	if !nonzero(l, 0, 10*320) {
		t.Fatal("jitter inserted silence into continuous audio")
	}
	if !zero(l, 10*320, 10*320+200*16) || !nonzero(l, len(l)-320, len(l)) {
		t.Fatal("a real pause must stay silent and the next frame must follow it")
	}
}

func TestCapsTruncate(t *testing.T) {
	for name, cfg := range map[string]Config{
		"duration": {MaxDuration: time.Second},
		"bytes":    {MaxBytes: 44 + 4*16000},
	} {
		t.Run(name, func(t *testing.T) {
			r, c, dir := newRec(t, cfg)
			for i := 1; i <= 3; i++ {
				c.Set(time.Duration(i*600) * time.Millisecond)
				r.User(in(600, 1))
			}
			c.Set(3 * time.Second)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			_, l, _ := wav(t, filepath.Join(dir, "a1-7-1.wav"))
			if len(l) != 16000 {
				t.Fatalf("samples=%d", len(l))
			}
			var sc Sidecar
			b, _ := os.ReadFile(filepath.Join(dir, "a1-7-1.json"))
			_ = json.Unmarshal(b, &sc)
			if !sc.Truncated {
				t.Fatal("truncated not flagged")
			}
		})
	}
}

func TestFinalizeIsAtomicAndTeardownKeepsPartial(t *testing.T) {
	r, c, dir := newRec(t, Config{FlushEvery: 10 * time.Millisecond})
	c.Set(time.Second)
	r.User(in(500, 7))
	c.Set(3 * time.Second)
	time.Sleep(100 * time.Millisecond) // a tick writes the body to the .part
	if _, err := os.Stat(filepath.Join(dir, "a1-7-1.wav")); err == nil {
		t.Fatal("final file visible before finalize")
	}
	if fi, err := os.Stat(filepath.Join(dir, "a1-7-1.wav.part")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("part: %v %v", fi, err)
	}
	if err := r.Close(); err != nil { // teardown mid-call
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a1-7-1.wav.part")); err == nil {
		t.Fatal(".part left behind")
	}
	wav(t, filepath.Join(dir, "a1-7-1.wav"))
	if fi, _ := os.Stat(filepath.Join(dir, "a1-7-1.wav")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
	r.User(in(10, 1)) // after close: no-op, no panic
}

type blockSink struct{ release chan struct{} }
type blockObj struct{ s *blockSink }

func (b blockSink) Create(string) (Object, error) { return blockObj{&b}, nil }
func (o blockObj) Write([]byte) (int, error)      { <-o.s.release; return 0, errors.New("late") }
func (o blockObj) Finalize(_, _ []byte) error     { return nil }
func (o blockObj) Abort() error                   { return nil }

func TestSlowSinkNeverStallsTaps(t *testing.T) {
	rel := make(chan struct{})
	defer close(rel)
	c := newClock()
	r := New(blockSink{rel}, Meta{ID: "x"}, Config{QueueLen: 4, FlushEvery: time.Millisecond, CloseTimeout: 100 * time.Millisecond, Now: c.Now})
	c.Set(5 * time.Second)
	r.User(in(20, 1)) // lets a tick try to write and block
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			r.User(in(20, 1))
			r.Bot(out(20, 1))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("taps stalled on a slow sink")
	}
	if r.Dropped() == 0 {
		t.Fatal("expected drops")
	}
	if err := r.Close(); !errors.Is(err, ErrCloseTimeout) {
		t.Fatalf("close: %v", err)
	}
}

type failSink struct{}

func (failSink) Create(string) (Object, error) { return nil, errors.New("disk full") }

func TestFailingSinkIsIsolated(t *testing.T) {
	c := newClock()
	r := New(failSink{}, Meta{ID: "x"}, Config{FlushEvery: time.Millisecond, Now: c.Now})
	c.Set(2 * time.Second)
	r.User(in(100, 1))
	time.Sleep(20 * time.Millisecond)
	r.User(in(100, 1)) // tap after failure must be a harmless no-op
	if err := r.Close(); err == nil {
		t.Fatal("want sink error")
	}
}

func TestPathSafety(t *testing.T) {
	s, _ := NewLocalSink(t.TempDir())
	for _, id := range []string{"", "../x", "a/b", `a\b`, "a.b", "a b", ".."} {
		if _, err := s.Create(id); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("%q accepted: %v", id, err)
		}
	}
	if o, err := s.Create("ok-1_x"); err != nil {
		t.Fatal(err)
	} else {
		_ = o.Abort()
	}
}

func TestSweepExpiresAndRecoversCrashedPartial(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewLocalSink(dir)
	now := time.Now()
	mk := func(name string, age time.Duration, body []byte) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	day := 24 * time.Hour
	mk("old.wav", 8*day, []byte("x"))
	mk("old.json", 8*day, []byte("{}"))
	mk("new.wav", day, []byte("x"))
	mk("other.txt", 30*day, []byte("keep"))
	mk("notes.v1.json", 30*day, []byte("keep")) // operator file: not a recording name
	mk("planted.wav.part", time.Hour, make([]byte, 4096))
	if err := os.Symlink(filepath.Join(dir, "other.txt"), filepath.Join(dir, "link.wav")); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(dir, "other.txt"), now.Add(-30*day), now.Add(-30*day))
	mk("crash.wav.part", time.Hour, append(wavHeader(16000, 0), make([]byte, 4*100+3)...))
	n, err := s.Sweep(now, 7*day, nil)
	if err != nil || n != 2 {
		t.Fatalf("removed=%d err=%v", n, err)
	}
	for _, name := range []string{"old.wav", "old.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatal(name, "survived")
		}
	}
	for _, name := range []string{"new.wav", "other.txt", "notes.v1.json", "link.wav", "planted.wav.part"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(name, "removed")
		}
	}
	_, l, _ := wav(t, filepath.Join(dir, "crash.wav"))
	if len(l) != 100 {
		t.Fatalf("recovered %d samples", len(l))
	}
}

func TestSinkRefusesSymlinkedDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "rec")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalSink(link); err == nil {
		t.Fatal("symlinked recording dir accepted")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Fatalf("target chmodded to %v", fi.Mode().Perm())
	}
	// A pre-existing permissive dir is tightened.
	open := filepath.Join(base, "open")
	_ = os.Mkdir(open, 0o777)
	if _, err := NewLocalSink(open); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(open); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
}

func TestSweepSparesRecordingsKeepNamesAndRemoveDeletesOnlyThatRecording(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewLocalSink(dir)
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	for _, name := range []string{"held.wav", "held.json", "gone.wav", "gone.json", "other.wav", "gone.wav.part"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
	}
	n, err := s.Sweep(now, time.Hour, func(id string) bool { return id == "held" })
	if err != nil || n != 4 {
		t.Fatalf("removed=%d err=%v", n, err)
	}
	for _, name := range []string{"held.wav", "held.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(name, "removed despite keep")
		}
	}
	// Remove takes the audio and the sidecar of one id and nothing else; an absent id is not an error.
	if n, err := s.Remove("held"); err != nil || n != 2 {
		t.Fatalf("remove = %d, %v", n, err)
	}
	if n, err := s.Remove("held"); err != nil || n != 0 {
		t.Fatalf("second remove = %d, %v", n, err)
	}
	for _, bad := range []string{"", "../x", "a/b", "x.wav"} {
		if _, err := s.Remove(bad); !errors.Is(err, ErrInvalidID) {
			t.Fatalf("Remove(%q) = %v", bad, err)
		}
	}
	// a symlink named like a recording is not followed or removed
	if err := os.Symlink(filepath.Join(dir, "other.wav"), filepath.Join(dir, "link.wav")); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Remove("link"); n != 0 {
		t.Fatal("removed a symlink")
	}
}
