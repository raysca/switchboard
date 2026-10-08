package main

import (
	"fmt"
	"sync"
	"time"
)

// shape is what a scenario does, in provider-request terms. estCalls turns it into a deliberately
// PESSIMISTIC upper bound, calibrated on measured runs (the first xAI S2/S4 run was estimated at 15
// and made ~30-35: xAI TTS sends one request per sentence, STT streams are adopted/rebound/reopened).
type shape struct {
	turns    int  // user turns (or bot-initiated replies) that reach the LLM and TTS
	extraSTT int  // extra STT stream opens: barge-in or resumed speech
	extraLLM int  // extra LLM requests: a tool round, a retry
	noSTT    bool // text-only: no audio, so no STT stream
}

// ttsPerReply is the TTS requests one reply can cost. Unit-based TTS (xAI unary, Google) sends one
// request per sentence and the standard agent answers in 2-4 sentences; a streaming socket needs 1-2.
func ttsPerReply(provider string) int {
	switch provider {
	case "xai":
		return 5
	case "google":
		return 4
	}
	return 2
}

// sttPerTurn: a turn adopts one stream and the stop path pre-opens the next, so 2 per turn.
const sttPerTurn = 2

// estCalls is the upper-bound provider-request estimate of one scenario run on profile p.
func estCalls(sh shape, p Profile) int {
	if p.Fake {
		return 0
	}
	stt := 0
	if !sh.noSTT {
		stt = 1 + sttPerTurn*sh.turns + sh.extraSTT // 1: the stream opened at call start
	}
	llm := sh.turns + sh.extraLLM
	tts := sh.turns * ttsPerReply(p.Stages.TTS.Provider)
	return stt + llm + tts
}

// spend tracks the measured provider requests of a run against -max-calls. A nil *spend never limits.
type spend struct {
	mu              sync.Mutex
	max, base, done int // limit; clip synthesis; requests of servers already stopped
}

func newSpend(max, base int) *spend { return &spend{max: max, base: base} }

// total is everything spent so far plus cur, the running server's count.
func (s *spend) total(cur int) int {
	if s == nil {
		return cur
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.base + s.done + cur
}

// finish folds a stopped server's final count in.
func (s *spend) finish(cur int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.done += cur
	s.mu.Unlock()
}

// over: the limit is exceeded. exhausted: nothing more may start (the next request would exceed it).
func (s *spend) over(cur int) bool      { return s != nil && s.max > 0 && s.total(cur) > s.max }
func (s *spend) exhausted(cur int) bool { return s != nil && s.max > 0 && s.total(cur) >= s.max }

// watchSpend polls read every interval and calls onOver (once) as soon as the measured total
// exceeds the limit, or as soon as read cannot measure (ok false): fail closed, an unreadable
// counter is never "unlimited". The returned stop ends the poller.
func watchSpend(s *spend, read func() (int, bool), interval time.Duration, onOver func()) (stop func()) {
	if s == nil || s.max <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if n, ok := read(); !ok || s.over(n) {
					onOver()
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// reqDelta is the per provider/stage requests between two scrapes.
func reqDelta(before, after map[string]int) map[string]int {
	out := map[string]int{}
	for k, a := range after {
		if d := a - before[k]; d > 0 {
			out[k] = d
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *spend) String() string { return fmt.Sprintf("%d/%d", s.total(0), s.max) }
