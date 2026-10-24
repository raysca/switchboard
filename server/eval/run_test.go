package eval

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/transport/ws"
)

// fakeTarget serves RunCall from an in-process RTVI server: it greets, answers every typed line with
// a reply, and sends an RTVI error message after a line that says "boom".
type fakeTarget struct {
	addr   string
	stored Stored
}

func newFakeTarget(t *testing.T, errText string, st Stored) *fakeTarget {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		send := func(typ, data string) {
			f, _ := ws.EncodeMessageFrame(`{"id":"s","label":"rtvi-ai","type":"` + typ + `","data":` + data + `}`)
			_ = c.Write(r.Context(), websocket.MessageBinary, f)
		}
		reply := func(text string) {
			send("bot-llm-started", `{}`)
			send("bot-llm-text", `{"text":`+string(must(json.Marshal(text)))+`}`)
			send("bot-llm-stopped", `{}`)
		}
		if _, _, err := c.Read(r.Context()); err != nil { // client-ready
			return
		}
		send("bot-ready", `{}`)
		reply("Welcome.")
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			f, err := ws.DecodeBinaryFrame(data, 1, time.Now())
			if m, ok := f.(*ws.WireMessageFrame); err == nil && ok && strings.Contains(m.Data, "send-text") {
				reply("Reply.")
				if strings.Contains(m.Data, "boom") {
					send("error", `{"error":"`+errText+`"}`)
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return &fakeTarget{addr: strings.TrimPrefix(srv.URL, "http://"), stored: st}
}

func must(b []byte, _ error) []byte { return b }

func (f *fakeTarget) DraftToken(context.Context, Agent) (string, error) {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return seg(`{}`) + "." + seg(`{"jti":"s1"}`) + ".sig", nil
}
func (f *fakeTarget) PublicAddr() string                             { return f.addr }
func (f *fakeTarget) Stored(context.Context, string) (Stored, error) { return f.stored, nil }

func runFake(t *testing.T, errText, endReason string, script ...string) (Observed, Input) {
	t.Helper()
	st := storedCall(true, "succeeded")
	st.Session.EndReason = endReason
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	obs, err := RunCall(ctx, newFakeTarget(t, errText, st), testAgent, ScriptCaller(script), Opts{TurnTimeout: 5 * time.Second, Quiet: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("RunCall: %v", err)
	}
	return obs, BuildInput(testAgent, obs, nil)
}

// RunCall's client facts reach the scorers: an RTVI error message is counted and shows as an
// "errors:" failure, so the always-on check cannot silently stop working.
func TestRunCallCountsRTVIErrors(t *testing.T) {
	obs, in := runFake(t, "llm_failed", "caller_hung_up", "hello", "boom")
	if obs.ClientErrors != 1 || in.ClientErrors != 1 {
		t.Fatalf("client errors = %d (input %d), want 1", obs.ClientErrors, in.ClientErrors)
	}
	if want := []Line{{"agent", "Welcome."}, {"caller", "hello"}, {"agent", "Reply."}, {"caller", "boom"}, {"agent", "Reply."}}; !equalLines(obs.Transcript, want) {
		t.Errorf("transcript = %v", obs.Transcript)
	}
	if f := failLines(Case{Name: "c"}, in); len(f) != 1 || f[0] != "errors: want 0 RTVI error messages, got 1" {
		t.Errorf("failures = %v", f)
	}
}

// The closing error message named after the stored end reason is the call's own end, not a fault.
func TestRunCallIgnoresTheEndReasonMessage(t *testing.T) {
	obs, in := runFake(t, "caller_hung_up", "caller_hung_up", "boom")
	if obs.ClientErrors != 0 || in.ClientErrors != 0 {
		t.Errorf("client errors = %d, want 0", obs.ClientErrors)
	}
}

func equalLines(a, b []Line) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type brokenTarget struct{ fakeTarget }

func (*brokenTarget) Stored(context.Context, string) (Stored, error) {
	return Stored{}, errors.New("GET sessions/s1: 503 Service Unavailable")
}

// A stored read that keeps failing is a harness error that says why, not a silent "did not end".
func TestFetchStoredReportsReadErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_, err := fetchStored(ctx, &brokenTarget{}, "s1")
	if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Errorf("err = %v", err)
	}
}
