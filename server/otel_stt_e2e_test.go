//go:build xaispeech

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A call's STT stream reaches OpenTelemetry as a govoice.stt span through the real server, and the
// transcript (echo's placeholder word "hello") stays out of the export by default.
func TestFlowE2E_OTelExportsSTTSpan(t *testing.T) {
	var mu sync.Mutex
	var body strings.Builder
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body.Write(b)
		mu.Unlock()
	}))
	defer collector.Close()

	r := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Providers.Echo.Mimic, c.Providers.Echo.STT = "google", "audio"
		c.OTel.Enabled, c.OTel.Endpoint = true, collector.URL+"/v1/traces"
	})
	realVAD(t, r.s)
	c := r.dial(interruptionFlow(t, r, "otelstt", ""))
	c.sendAudio(callerSpeech(t))
	within(t, 20*time.Second, "the caller's turn to finish", func() bool { return countOf(c.wireLog(), "audio") > 0 })
	_ = c.conn.CloseNow()
	<-c.done
	if err := r.s.otel.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := body.String()
	mu.Unlock()
	if !strings.Contains(got, "govoice.stt") {
		t.Fatalf("no govoice.stt span exported (%d bytes)", len(got))
	}
	if strings.Contains(got, "hello") {
		t.Fatal("transcript text reached the OTel export")
	}
}
