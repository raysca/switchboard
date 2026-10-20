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

// The unmodified pinned Pipecat client, connected to the real server, fires its function-call callbacks in order
// with the ids only.
func TestMCPCallPipecatClientSeesFunctionCallEvents(t *testing.T) {
	compat := filepath.Join("..", "tools", "compat")
	if _, err := os.Stat(filepath.Join(compat, "node_modules", "@pipecat-ai", "client-js")); err != nil {
		t.Fatalf("the pinned Pipecat client is not installed (run: npm ci --prefix tools/compat): %v", err)
	}
	r := newMCPCallRig(t)
	id, hashes := r.server("ok", r.ts.URL+"/ok", "MCP_TOKEN_TEST")
	a := r.flowWith(id, hashes, true, nil, "lookup_order")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "node", filepath.Join(compat, "test_rtvi_function_calls.js"),
		// r.ts is the mcptest server (mcpCallRig shadows the embedded flowRigE2E.ts): dial the voice server.
		"--url", "ws"+strings.TrimPrefix(r.flowRigE2E.ts.URL, "http"), "--token", r.voiceToken(a.ID),
		"--text", `call tool lookup_order {"order_id":"42"}`).CombinedOutput()
	if err != nil {
		t.Fatalf("client-js run failed: %v\n%s", err, out)
	}
	_, line, ok := strings.Cut(string(out), "EVENTS ")
	if !ok {
		t.Fatalf("no EVENTS line:\n%s", out)
	}
	line, _, _ = strings.Cut(line, "\n")
	var got struct {
		Events  []fnEvent `json:"events"`
		Emitted []string  `json:"emitted"`
		TTS     []string  `json:"tts"`
	}
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("EVENTS %s: %v", line, err)
	}
	want := []string{`started{}`, `in-progress{"tool_call_id":"#1"}`, `stopped{"cancelled":false,"tool_call_id":"#1"}`}
	if shape := fnShape(t, got.Events); !slices.Equal(shape, want) {
		t.Fatalf("client callbacks %q, want %q", shape, want)
	}
	if len(got.Emitted) != 1 || got.Emitted[0] != got.Events[1].Data["tool_call_id"] {
		t.Fatalf("event emitter saw %q", got.Emitted)
	}
	// The spoken text of each unit arrives as bot-tts-text after its audio; the barrier turn's reply contains "barrier".
	if !slices.ContainsFunc(got.TTS, func(s string) bool { return strings.Contains(s, "barrier") }) || slices.Contains(got.TTS, "") {
		t.Fatalf("bot-tts-text payloads %q: want one containing \"barrier\" and none empty", got.TTS)
	}
}
