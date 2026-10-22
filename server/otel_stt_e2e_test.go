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

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// A call's STT stream reaches OpenTelemetry as a govoice.stt span through the real server, and the
// transcript (echo's placeholder word "hello") stays out of the export by default.
func TestFlowE2E_OTelExportsSTTSpan(t *testing.T) {
	var mu sync.Mutex
	var body strings.Builder
	var statuses []tracepb.Status_StatusCode // one per exported govoice.stt span
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body.Write(b)
		var req collectortrace.ExportTraceServiceRequest
		if proto.Unmarshal(b, &req) == nil {
			for _, rs := range req.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						if sp.Name == "govoice.stt" {
							statuses = append(statuses, sp.GetStatus().GetCode())
						}
					}
				}
			}
		}
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
	sttStatuses := append([]tracepb.Status_StatusCode(nil), statuses...)
	mu.Unlock()
	if !strings.Contains(got, "govoice.stt") {
		t.Fatalf("no govoice.stt span exported (%d bytes)", len(got))
	}
	if strings.Contains(got, "hello") {
		t.Fatal("transcript text reached the OTel export")
	}
	// Streams open on any audio, so one is always still open at hangup: ending with the call is
	// not an STT failure.
	if len(sttStatuses) == 0 {
		t.Fatal("no govoice.stt span decoded from the export")
	}
	for _, c := range sttStatuses {
		if c == tracepb.Status_STATUS_CODE_ERROR {
			t.Fatalf("a normal call exported an errored govoice.stt span: %v", sttStatuses)
		}
	}
}
