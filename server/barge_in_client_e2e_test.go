package server

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The unmodified pinned client, connected to the real server, takes a barge-in without an error: it handles
// bot-interrupted once (as a type it does not recognise) after the cut reply's audio, and logs no frame it
// could not deserialize (the protobuf interruption frame would be one). It does not clear its player on it:
// that limitation is recorded in docs/compatibility/playback-clear-decision.json.
func TestCallBehaviorPipecatClientTakesABargeInWithoutErrors(t *testing.T) {
	compat := filepath.Join("..", "tools", "compat")
	if _, err := os.Stat(filepath.Join(compat, "node_modules", "@pipecat-ai", "client-js")); err != nil {
		t.Fatalf("the pinned Pipecat client is not installed (run: npm ci --prefix tools/compat): %v", err)
	}
	r := bargeInRig(t)
	agentID := behaviorAgent(t, r, "Talker", "talkvoice", ``)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "node", filepath.Join(compat, "test_rtvi_barge_in.js"),
		"--url", "ws"+strings.TrimPrefix(r.ts.URL, "http"), "--token", r.voiceToken(agentID),
		"--long", longReply, "--barge", "barrier").CombinedOutput()
	if err != nil {
		t.Fatalf("client-js run failed: %v\n%s", err, out)
	}
	_, line, ok := strings.Cut(string(out), "EVENTS ")
	if !ok {
		t.Fatalf("no EVENTS line:\n%s", out)
	}
	line, _, _ = strings.Cut(line, "\n")
	var got struct {
		Log        []string `json:"log"`
		Interrupts int      `json:"interrupts"`
		Errors     []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("EVENTS %s: %v", line, err)
	}
	if len(got.Errors) != 0 {
		t.Fatalf("the pinned client logged errors: %q", got.Errors)
	}
	if n := countOf(got.Log, "msg:bot-interrupted"); n != 1 {
		t.Fatalf("the client handled bot-interrupted %d times, want 1: %v", n, got.Log)
	}
	if at := slices.Index(got.Log, "msg:bot-interrupted"); countOf(got.Log[:at], "audio") < 10 {
		t.Fatalf("bot-interrupted came before the cut reply played: %v", got.Log)
	}
	t.Logf("the pinned client asked its player to interrupt %d time(s) (0 is the recorded limitation)", got.Interrupts)
}
