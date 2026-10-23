package server

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// wireLog is what the client received so far: "msg:<type>" per RTVI message, "audio" per bot audio frame.
func (c *e2eCall) wireLog() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.wire)
}

func countOf(log []string, entry string) int {
	n := 0
	for _, e := range log {
		if e == entry {
			n++
		}
	}
	return n
}

// indexFrom is the index of entry in log at or after from; -1 if absent or from < 0.
func indexFrom(log []string, entry string, from int) int {
	if from < 0 {
		return -1
	}
	for i := from; i < len(log); i++ {
		if log[i] == entry {
			return i
		}
	}
	return -1
}

// longReply is ten short sentences: with Google-like echo timing (bargeInRig) the first sentence is spoken
// within a second and the whole reply plays for several seconds, so a later turn cuts it off mid-play.
var longReply = strings.TrimSpace(strings.Repeat("This sentence keeps the reply going. ", 10))

// bargeInRig is the agent rig with the echo providers on Google-like timing: "instant" synthesis makes two
// 20 ms frames per piece, too little audio for a reply to still be playing when the next turn arrives.
func bargeInRig(t *testing.T) *agentRigE2E {
	return newAgentRigE2E(t, newProbe(), func(c *Config) { c.Providers.Echo.Mimic = "google" })
}

// within is eventually with its own deadline: on Google-like timing one barge-in test takes about 7 s, so the
// rig's 8 s limit is too close for the waits that span a whole reply.
func within(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out after %v waiting for %s", d, what)
		}
	}
}

// A typed turn that cuts a playing reply off: bot-interrupted arrives once, no audio of the cut reply follows
// it (at most the frame already on the socket) and the new reply comes after it. The first turn, sent while
// nothing played, sends none (its InterruptionFrame reaches the transport without a flush).
func TestCallBehaviorBargeInSendsBotInterruptedOnceAfterTheCutReplysAudio(t *testing.T) {
	r := bargeInRig(t)
	c := r.dial(behaviorAgent(t, r, "Talker", "talkvoice", ``))
	c.say(longReply)
	within(t, 20*time.Second, "the long reply to play", func() bool { return countOf(c.wireLog(), "audio") >= 10 })
	if n := countOf(c.wireLog(), "msg:bot-interrupted"); n != 0 {
		t.Fatalf("bot-interrupted with nothing cut off: %d", n)
	}
	c.say("barrier")
	c.waitReplies("barrier", 1)
	within(t, 20*time.Second, "the barrier reply to finish playing", func() bool {
		log := c.wireLog()
		return indexFrom(log, "msg:bot-stopped-speaking", indexFrom(log, "msg:bot-tts-started", indexFrom(log, "msg:bot-interrupted", 0))) >= 0
	})
	log := c.wireLog()
	if n := countOf(log, "msg:bot-interrupted"); n != 1 {
		t.Fatalf("bot-interrupted %d times, want 1: %v", n, log)
	}
	at := indexFrom(log, "msg:bot-interrupted", 0)
	next := indexFrom(log, "msg:bot-tts-started", at)
	if n := countOf(log[at:next], "audio"); n > 1 {
		t.Fatalf("%d audio frames between bot-interrupted and the next reply (at most the one already on the socket): %v", n, log[at:next+1])
	}
	// A third turn once the assistant is idle cuts nothing off: no second bot-interrupted, now or late.
	before := len(log)
	c.say("third")
	c.waitReplies("third", 1)
	within(t, 20*time.Second, "the third reply to finish playing", func() bool {
		return indexFrom(c.wireLog(), "msg:bot-stopped-speaking", indexFrom(c.wireLog(), "msg:bot-tts-started", before)) >= 0
	})
	if n := countOf(c.wireLog(), "msg:bot-interrupted"); n != 1 {
		t.Fatalf("bot-interrupted %d times after an idle turn, want 1: %v", n, c.wireLog())
	}
	if c.ended() {
		t.Fatal("the barge-in ended the call")
	}
}
