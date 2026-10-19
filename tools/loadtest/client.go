package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/tools/internal/stack"
	"govoice/transport/ws"
)

// lagBounds are the harness pacing-lag histogram bounds in ms.
var lagBounds = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000}

// LoadState aggregates client-side results across slots.
type LoadState struct {
	mu            sync.Mutex
	v2v           []float64
	errs          map[string]int
	chaosErrs     map[string]int
	sessions      SessionCounts
	turns         TurnCounts
	cur, max      atomic.Int64
	lag           [11]atomic.Uint64 // len(lagBounds)+1
	sessNS        atomic.Int64      // total connected session time
	chaosInjected atomic.Uint64
}

func newLoadState() *LoadState {
	return &LoadState{
		errs:      map[string]int{},
		chaosErrs: map[string]int{},
	}
}

func (l *LoadState) addErr(k string) {
	l.mu.Lock()
	l.errs[k]++
	l.mu.Unlock()
}

func (l *LoadState) recordChaos(kind string) {
	l.chaosInjected.Add(1)
	l.mu.Lock()
	if l.chaosErrs == nil {
		l.chaosErrs = map[string]int{}
	}
	l.chaosErrs[kind]++
	l.mu.Unlock()
}

// callFailed records a call-level failure.
func (l *LoadState) callFailed(kind string) {
	l.mu.Lock()
	l.errs[kind]++
	l.sessions.Failed++
	l.mu.Unlock()
}

func (l *LoadState) observeLag(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	i := 0
	for i < len(lagBounds) && ms > lagBounds[i] {
		i++
	}
	l.lag[i].Add(1)
}

// lagStat is bucket-resolution (upper bound reported).
func (l *LoadState) lagStat() Stat {
	var total uint64
	counts := make([]uint64, len(l.lag))
	for i := range l.lag {
		counts[i] = l.lag[i].Load()
		total += counts[i]
	}
	q := func(p float64) float64 {
		rank, cum := uint64(float64(total)*p/100+0.999999), uint64(0)
		for i, c := range counts {
			cum += c
			if cum >= rank && i < len(lagBounds) {
				return lagBounds[i]
			}
			if cum >= rank {
				break
			}
		}
		return lagBounds[len(lagBounds)-1] * 2
	}
	if total == 0 {
		return Stat{}
	}
	return Stat{N: int(total), P50: q(50), P95: q(95), P99: q(99), Max: q(100)}
}

// CallParams describes the simulated caller.
type CallParams struct {
	Server      *Server
	FlowID      string
	Clip        []byte
	Turns       int // 0 = until ctx ends
	MinTurns    int
	MaxTurns    int
	MinDuration time.Duration
	MaxDuration time.Duration
	Think       time.Duration
	TurnLimit   time.Duration // max wait for first bot audio
	IdleRSSMB   float64       // RSS of the freshly started server
	ChaosRate   float64       // probability of injecting client fault
}

// call is one WebSocket call with a real-time 20 ms mic stream.
type call struct {
	conn     *websocket.Conn
	st       *LoadState
	next     time.Time
	seq      int
	audioCh  chan time.Time // first bot audio after awaiting is armed
	await    atomic.Bool
	speaking atomic.Bool   // bot audio in flight (set by audio frames, cleared by bot-stopped-speaking)
	stopCh   chan struct{} // bot-stopped-speaking
	errCh    chan error
}

func (c *call) pace(ctx context.Context, chunk []byte) error {
	if d := time.Until(c.next); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	c.st.observeLag(max(time.Since(c.next), 0)) // how late this 20 ms send is: harness health
	c.next = c.next.Add(chunkMS * time.Millisecond)
	frame, err := ws.EncodeAudioFrame(chunk, sampleRate, 1)
	if err != nil {
		return err
	}
	// ponytail: Background, not ctx: cancelling a coder/websocket op kills the connection, and a
	// graceful close at the end of the run is what a real client does (abrupt drops mid-reply
	// are counted by the server as slow-client failures). The loop checks ctx between chunks.
	return c.conn.Write(context.Background(), websocket.MessageBinary, frame)
}

var silence = make([]byte, chunkBytes)

// idle streams silence for d, returning early if early() is true.
func (c *call) idle(ctx context.Context, d time.Duration, early func() bool) error {
	for end := time.Now().Add(d); time.Now().Before(end); {
		if early != nil && early() {
			return nil
		}
		select {
		case err := <-c.errCh:
			return err
		default:
		}
		if err := c.pace(ctx, silence); err != nil {
			return err
		}
	}
	return nil
}

func (c *call) reader(ctx context.Context) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			select {
			case c.errCh <- err:
			default:
			}
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		c.seq++
		f, err := ws.DecodeBinaryFrame(data, uint64(c.seq), time.Now())
		if err != nil {
			continue
		}
		switch fr := f.(type) {
		case *core.InputAudioFrame:
			c.speaking.Store(true)
			if c.await.CompareAndSwap(true, false) {
				select { // never block the reader: a stalled reader stops draining the socket
				case c.audioCh <- time.Now():
				default:
				}
			}
		case *ws.WireMessageFrame:
			if strings.Contains(fr.Data, `"bot-stopped-speaking"`) {
				c.speaking.Store(false)
				select {
				case c.stopCh <- struct{}{}:
				default:
				}
			} else if strings.Contains(fr.Data, `"type":"error"`) {
				c.st.addErr("server_error_message")
			}
		}
	}
}

// runCall runs one call: handshake, then p.Turns speech turns. It returns the
// error that ended the call early (nil when finished or ctx ended).
func runCall(ctx context.Context, st *LoadState, p CallParams, principal string) error {
	tok, err := p.Server.Token(principal, p.FlowID)
	if err != nil {
		st.callFailed("token")
		return err
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, resp, err := websocket.Dial(dctx, "ws://"+p.Server.Public+"/ws?token="+tok, nil)
	cancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == 429 || resp.StatusCode == 503) {
			st.mu.Lock()
			st.sessions.Rejected++
			st.mu.Unlock()
			st.addErr(fmt.Sprintf("rejected_http_%d", resp.StatusCode))
		} else if ctx.Err() == nil {
			st.callFailed("dial")
		}
		return errors.New("dial failed")
	}
	defer conn.CloseNow() // handshake failures; no-op after the graceful close below
	conn.SetReadLimit(1 << 20)
	rctx, rcancel := context.WithCancel(context.Background()) // reader outlives ctx so the close handshake completes
	defer rcancel()

	cctx := ctx
	c := &call{conn: conn, st: st, audioCh: make(chan time.Time, 1), stopCh: make(chan struct{}, 1), errCh: make(chan error, 1)}
	if err := stack.Handshake(ctx, conn, false); err != nil {
		if ctx.Err() == nil {
			st.callFailed("handshake")
		}
		return err
	}
	go c.reader(rctx)
	defer func() { // graceful end even when the run stops mid-call: let a reply in flight finish, then close
		for i := 0; i < 150 && c.speaking.Load(); i++ {
			time.Sleep(20 * time.Millisecond)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	st.mu.Lock()
	st.sessions.Started++
	st.mu.Unlock()
	n := st.cur.Add(1)
	for m := st.max.Load(); n > m && !st.max.CompareAndSwap(m, n); m = st.max.Load() {
	}
	t0 := time.Now()
	defer func() {
		st.cur.Add(-1)
		st.sessNS.Add(int64(time.Since(t0)))
	}()

	// Determine if this call should undergo injected chaos
	var chaosMode string
	if p.ChaosRate > 0 && rand.Float64() < p.ChaosRate {
		modes := []string{"tcp_abort", "malformed_frame", "stalled_consumer"}
		chaosMode = modes[rand.Intn(len(modes))]
	}

	// Target turns and deadline for call churn
	targetTurns := p.Turns
	if p.MinTurns > 0 && p.MaxTurns >= p.MinTurns {
		targetTurns = p.MinTurns + rand.Intn(p.MaxTurns-p.MinTurns+1)
	}
	var callDeadline time.Time
	if p.MinDuration > 0 && p.MaxDuration >= p.MinDuration {
		dur := p.MinDuration + time.Duration(rand.Int63n(int64(p.MaxDuration-p.MinDuration+1)))
		callDeadline = time.Now().Add(dur)
	}

	c.next = time.Now()
	fail := func(kind string, err error) error {
		if ctx.Err() != nil {
			return nil // run ended; not a failure
		}
		st.callFailed(kind)
		return err
	}
	if err := c.idle(cctx, 500*time.Millisecond, nil); err != nil {
		return fail("conn_lost", err)
	}
	for turn := 0; (targetTurns == 0 || turn < targetTurns) && ctx.Err() == nil; turn++ {
		if !callDeadline.IsZero() && time.Now().After(callDeadline) {
			break
		}
		if chaosMode != "" && turn == 0 {
			switch chaosMode {
			case "tcp_abort":
				_ = c.pace(cctx, p.Clip[:min(chunkBytes*2, len(p.Clip))])
				st.recordChaos("tcp_abort")
				conn.CloseNow()
				return nil
			case "malformed_frame":
				st.recordChaos("malformed_frame")
				_ = conn.Write(context.Background(), websocket.MessageBinary, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04})
				time.Sleep(100 * time.Millisecond)
				conn.CloseNow()
				return nil
			case "stalled_consumer":
				st.recordChaos("stalled_consumer")
				rcancel() // kill reader so client stops reading
				for off := 0; off < min(len(p.Clip), chunkBytes*10); off += chunkBytes {
					chunk := p.Clip[off : off+chunkBytes]
					_ = c.pace(cctx, chunk)
				}
				time.Sleep(1 * time.Second)
				conn.CloseNow()
				return nil
			}
		}
		for len(c.audioCh) > 0 {
			<-c.audioCh
		}
		select {
		case <-c.stopCh:
		default:
		}
		for off := 0; off < len(p.Clip); off += chunkBytes {
			chunk := p.Clip[off:min(off+chunkBytes, len(p.Clip))]
			if len(chunk) < chunkBytes {
				chunk = append(append([]byte(nil), chunk...), silence[:chunkBytes-len(chunk)]...)
			}
			if err := c.pace(cctx, chunk); err != nil {
				return fail("conn_lost", err)
			}
		}
		tEnd := time.Now()
		c.await.Store(true)
		st.mu.Lock()
		st.turns.Attempted++
		st.mu.Unlock()
		var got bool
		var tFirst time.Time
		for end := tEnd.Add(p.TurnLimit); time.Now().Before(end) && !got; {
			select {
			case tFirst = <-c.audioCh:
				got = true
				continue
			default:
			}
			if err := c.idle(cctx, chunkMS*time.Millisecond, nil); err != nil {
				return fail("conn_lost", err)
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		if !got {
			c.await.Store(false)
			st.mu.Lock()
			st.turns.Timeouts++
			// Censored at the limit, not dropped: excluding the worst turns from
			// the percentiles would hide exactly the saturation the run is for.
			st.v2v = append(st.v2v, float64(p.TurnLimit)/float64(time.Millisecond))
			st.mu.Unlock()
			st.addErr("turn_timeout")
			continue
		}
		st.mu.Lock()
		st.turns.Completed++
		st.v2v = append(st.v2v, float64(tFirst.Sub(tEnd))/float64(time.Millisecond))
		st.mu.Unlock()
		stopped := false
		if err := c.idle(cctx, 3*time.Second, func() bool {
			select {
			case <-c.stopCh:
				stopped = true
			default:
			}
			return stopped
		}); err != nil {
			return fail("conn_lost", err)
		}
		if err := c.idle(cctx, p.Think, nil); err != nil {
			return fail("conn_lost", err)
		}
	}
	if ctx.Err() == nil {
		st.mu.Lock()
		st.sessions.Completed++
		st.mu.Unlock()
	}
	return nil
}
