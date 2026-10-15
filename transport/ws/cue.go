package ws

import (
	"encoding/binary"
	"sync/atomic"
	"time"
)

// CueUnderlay mixes an optional background with a cue the server starts and
// stops at run time (the route gap sound). Mix, Active and Rate run on the
// paced writer's goroutine; Start and Stop may be called from any goroutine.
type CueUnderlay struct {
	bg      atomic.Pointer[background] // the session's ambience; set at accept or later (SetBackground)
	cur     atomic.Pointer[cueState]
	wake    func() // nudges an idle pacer when a cue starts; set by the transport
	scratch []byte // Mix goroutine only; sized once
}

type cueState struct {
	u        Underlay
	fadeLen  int64        // samples; 0 = not fading
	fadeLeft atomic.Int64 // Mix goroutine only
}

// background boxes an Underlay so it can sit in an atomic pointer.
type background struct{ u Underlay }

func NewCueUnderlay() *CueUnderlay { return &CueUnderlay{} }

// SetBackground replaces the session's ambience (nil: none). Safe to call while the writer mixes, so a
// server that learns the call's flow after the connection was accepted can choose the sound then.
func (c *CueUnderlay) SetBackground(u Underlay) {
	if u == nil {
		c.bg.Store(nil)
	} else {
		c.bg.Store(&background{u})
	}
	if c.wake != nil {
		c.wake()
	}
}

func (c *CueUnderlay) bgActive() bool {
	b := c.bg.Load()
	return b != nil && b.u.Active()
}

func (c *CueUnderlay) cueActive() bool {
	s := c.cur.Load()
	return s != nil && s.u.Active() && (s.fadeLen == 0 || s.fadeLeft.Load() > 0)
}

func (c *CueUnderlay) Rate() int {
	if b := c.bg.Load(); b != nil {
		return b.u.Rate()
	}
	if s := c.cur.Load(); s != nil {
		return s.u.Rate()
	}
	return 0
}

func (c *CueUnderlay) Active() bool { return c.bgActive() || c.cueActive() }

// Start begins playing u over the background.
func (c *CueUnderlay) Start(u Underlay) {
	c.cur.Store(&cueState{u: u})
	if c.wake != nil {
		c.wake()
	}
}

// Stop ends the cue, fading over fade (0: at once).
func (c *CueUnderlay) Stop(fade time.Duration) {
	s := c.cur.Load()
	if s == nil {
		return
	}
	if fade <= 0 {
		c.cur.CompareAndSwap(s, nil)
		return
	}
	next := &cueState{u: s.u, fadeLen: max(int64(fade)*int64(s.u.Rate())/int64(time.Second), 1)}
	next.fadeLeft.Store(next.fadeLen)
	c.cur.CompareAndSwap(s, next)
}

// Mix fills dst with src (nil: silence) plus the background plus the cue.
func (c *CueUnderlay) Mix(dst, src []byte) {
	bg := c.bg.Load() // once: SetBackground may swap it between the check and the mix
	switch {
	case bg != nil && bg.u.Active():
		bg.u.Mix(dst, src)
	case src != nil:
		copy(dst, src)
	default:
		clear(dst)
	}
	s := c.cur.Load()
	if s == nil || !c.cueActive() {
		return
	}
	if cap(c.scratch) < len(dst) {
		c.scratch = make([]byte, len(dst)) // first cue frame only
	}
	buf := c.scratch[:len(dst)]
	s.u.Mix(buf, nil)
	left := s.fadeLeft.Load()
	for i := 0; i+1 < len(dst); i += 2 {
		cs := int32(int16(binary.LittleEndian.Uint16(buf[i:])))
		if s.fadeLen > 0 {
			if left > 0 {
				cs = int32(int64(cs) * left / s.fadeLen)
				left--
			} else {
				cs = 0
			}
		}
		v := int32(int16(binary.LittleEndian.Uint16(dst[i:]))) + cs
		binary.LittleEndian.PutUint16(dst[i:], uint16(int16(max(min(v, 32767), -32768))))
	}
	if s.fadeLen > 0 {
		s.fadeLeft.Store(left)
	}
}
