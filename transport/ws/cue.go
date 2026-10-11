package ws

import (
	"encoding/binary"
	"sync/atomic"
	"time"
)

// CueUnderlay mixes an optional background with a cue the server starts and
// stops at run time (the handoff gap sound). Mix, Active and Rate run on the
// paced writer's goroutine; Start and Stop may be called from any goroutine.
type CueUnderlay struct {
	bg      Underlay
	cur     atomic.Pointer[cueState]
	wake    func() // nudges an idle pacer when a cue starts; set by the transport
	scratch []byte // Mix goroutine only; sized once
}

type cueState struct {
	u        Underlay
	fadeLen  int64        // samples; 0 = not fading
	fadeLeft atomic.Int64 // Mix goroutine only
}

func NewCueUnderlay(background Underlay) *CueUnderlay { return &CueUnderlay{bg: background} }

func (c *CueUnderlay) bgActive() bool { return c.bg != nil && c.bg.Active() }

func (c *CueUnderlay) cueActive() bool {
	s := c.cur.Load()
	return s != nil && s.u.Active() && (s.fadeLen == 0 || s.fadeLeft.Load() > 0)
}

func (c *CueUnderlay) Rate() int {
	if c.bg != nil {
		return c.bg.Rate()
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
	switch {
	case c.bgActive():
		c.bg.Mix(dst, src)
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
