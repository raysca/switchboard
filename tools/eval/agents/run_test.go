package main

import (
	"encoding/base64"
	"testing"
	"time"

	"govoice/server/api"
	"govoice/tools/internal/stack"
)

func TestJTI(t *testing.T) {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tok := seg(`{"alg":"HS256"}`) + "." + seg(`{"jti":"01JSESSION","sub":"agent-eval"}`) + ".sig"
	if got, err := jti(tok); err != nil || got != "01JSESSION" {
		t.Fatalf("jti = %q, %v", got, err)
	}
	for _, bad := range []string{"", "a.b", "a.!!.c", seg("x") + "." + seg(`{}`) + ".s", seg("x") + "." + seg(`[1]`) + ".s"} {
		if _, err := jti(bad); err == nil {
			t.Errorf("jti(%q) accepted", bad)
		}
	}
}

func TestTurnState(t *testing.T) {
	t0 := time.Now()
	ev := func(kinds ...string) (out []stack.Event) {
		for i, k := range kinds {
			out = append(out, stack.Event{At: t0.Add(time.Duration(i+1) * time.Millisecond), Kind: k})
		}
		return out
	}
	for _, c := range []struct {
		name string
		kind []string
		open bool
	}{
		{"nothing", nil, false},
		{"text, reply still open", []string{"bot-llm-started", "bot-llm-text"}, true},
		{"reply closed", []string{"bot-llm-started", "bot-llm-text", "bot-llm-stopped"}, false},
		{"tool in flight", []string{"bot-llm-started", "bot-llm-stopped", "llm-function-call-started"}, true},
		{"tool done, follow-up reply not yet stopped", []string{"bot-llm-started", "bot-llm-stopped", "llm-function-call-started", "llm-function-call-stopped"}, true},
		{"tool done, follow-up reply stopped", []string{"llm-function-call-started", "llm-function-call-stopped", "bot-llm-started", "bot-llm-text", "bot-llm-stopped"}, false},
		{"model failed after a tool call: error closes the reply", []string{"bot-llm-started", "bot-llm-stopped", "llm-function-call-started", "llm-function-call-stopped", "error"}, false},
		{"error while a tool is still in flight", []string{"llm-function-call-started", "error"}, true},
		{"cut-off reply replaced by a closed one", []string{"bot-llm-started", "bot-llm-text", "bot-llm-started", "bot-llm-stopped"}, false},
	} {
		if _, _, open := turnState(ev(c.kind...), t0); open != c.open {
			t.Errorf("%s: open = %v, want %v", c.name, open, c.open)
		}
	}
	if _, text, _ := turnState(ev("bot-llm-text"), t0.Add(time.Second)); text {
		t.Error("events before the mark counted")
	}
}

func TestTracesComplete(t *testing.T) {
	span := func(n string) api.TraceSpanDTO { return api.TraceSpanDTO{Name: n} }
	call := func(n string) api.ToolCallDTO { return api.ToolCallDTO{RemoteName: n} }
	for _, c := range []struct {
		name string
		tr   api.SessionTracesDTO
		want bool
	}{
		{"no transfers", api.SessionTracesDTO{ToolCalls: []api.ToolCallDTO{call("end_call")}}, true},
		{"transfer, span late", api.SessionTracesDTO{ToolCalls: []api.ToolCallDTO{call("transfer_to_sales")}}, false},
		{"transfer, active span", api.SessionTracesDTO{ToolCalls: []api.ToolCallDTO{call("transfer_to_sales")}, Spans: []api.TraceSpanDTO{span("handoff.active")}}, true},
		{"transfer, failed span", api.SessionTracesDTO{ToolCalls: []api.ToolCallDTO{call("transfer_to_sales")}, Spans: []api.TraceSpanDTO{span("handoff.failed")}}, true},
		{"two transfers, one span", api.SessionTracesDTO{ToolCalls: []api.ToolCallDTO{call("transfer_to_a"), call("transfer_to_b")}, Spans: []api.TraceSpanDTO{span("handoff.active")}}, false},
	} {
		if got := tracesComplete(c.tr); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
