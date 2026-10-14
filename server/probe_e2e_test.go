package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"govoice/core"
)

const mgmtKey = "management-test-key-32-bytes-long-and-separate"

type testResponse struct {
	Data struct {
		Provider  string        `json:"provider"`
		CheckedAt string        `json:"checked_at"`
		Results   []ProbeResult `json:"results"`
	} `json:"data"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func testProvider(t *testing.T, s *Server, id string) (int, testResponse, string, string) {
	t.Helper()
	w := apiRequest(s, http.MethodPost, "/api/v1/providers/"+id+"/test", mgmtKey, "")
	var out testResponse
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out, w.Body.String(), w.Header().Get("Retry-After")
}

func probeServer(t *testing.T, env map[string]string) *Server {
	s := apiTestServer(t)
	s.env = mapEnv(env)
	installProviders(s)
	return s
}

func TestProviderTestChecksEveryServiceOfEcho(t *testing.T) {
	s := probeServer(t, nil)
	code, out, body, _ := testProvider(t, s, "echo")
	if code != http.StatusOK || out.Data.Provider != "echo" || out.Data.CheckedAt == "" {
		t.Fatalf("%d %s", code, body)
	}
	var stages []string
	for _, r := range out.Data.Results {
		stages = append(stages, r.Stage)
		if r.Status != "ok" || r.Circuit != "closed" {
			t.Errorf("%s: %+v", r.Stage, r)
		}
		if (r.Stage == "llm" || r.Stage == "tts") && r.LatencyMS == nil {
			t.Errorf("%s has no latency", r.Stage)
		}
	}
	if strings.Join(stages, ",") != "stt,llm,tts" {
		t.Fatalf("stages %v", stages)
	}
}

func TestProviderTestNeedsTheManagementKeyAndAKnownProvider(t *testing.T) {
	s := probeServer(t, nil)
	if w := apiRequest(s, http.MethodPost, "/api/v1/providers/echo/test", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", w.Code)
	}
	if code, out, _, _ := testProvider(t, s, "nope"); code != http.StatusNotFound || out.Error.Code != "not_found" {
		t.Fatalf("unknown: %d %+v", code, out)
	}
	w := apiRequest(s, http.MethodGet, "/api/v1/providers/echo/test", mgmtKey, "")
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET: %d allow=%q", w.Code, w.Header().Get("Allow"))
	}
}

func TestProviderTestIsRefusedWithoutACredentialAndSendsNothing(t *testing.T) {
	s := probeServer(t, nil)
	var built atomic.Int32
	for _, st := range []string{"stt", "llm", "tts"} {
		s.registry.Register("xai."+st, func(*Config, StageSpec) (core.Processor, error) {
			built.Add(1)
			return nil, errors.New("must not be built")
		})
	}
	code, out, _, _ := testProvider(t, s, "xai")
	if code != http.StatusConflict || out.Error.Code != "provider_not_detected" || built.Load() != 0 {
		t.Fatalf("%d %+v built=%d", code, out, built.Load())
	}
}

func TestProviderTestIsRateLimitedPerProvider(t *testing.T) {
	s := probeServer(t, nil)
	if code, _, body, _ := testProvider(t, s, "echo"); code != http.StatusOK {
		t.Fatalf("first: %d %s", code, body)
	}
	code, out, _, retry := testProvider(t, s, "echo")
	if code != http.StatusTooManyRequests || out.Error.Code != "probe_too_soon" || retry == "" {
		t.Fatalf("second: %d %+v retry=%q", code, out, retry)
	}
}

func TestProviderTestAllowsOnlyOneAtATime(t *testing.T) {
	s := probeServer(t, nil)
	var wg sync.WaitGroup
	var ok, limited atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch code, _, _, _ := testProvider(t, s, "echo"); code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || limited.Load() != 3 {
		t.Fatalf("ok=%d limited=%d", ok.Load(), limited.Load())
	}
}

func TestProviderTestNeverShowsASecret(t *testing.T) {
	const key = "sekrit-xai-value"
	s := probeServer(t, map[string]string{"XAI_API_KEY": key})
	for _, st := range []string{"stt", "llm", "tts"} {
		s.registry.Register("xai."+st, func(_ *Config, spec StageSpec) (core.Processor, error) {
			return nil, errors.New("401 for " + string(spec.Secret))
		})
	}
	code, out, body, _ := testProvider(t, s, "xai")
	if code != http.StatusOK || len(out.Data.Results) != 3 {
		t.Fatalf("%d %s", code, body)
	}
	for _, r := range out.Data.Results {
		if r.Status != "error" {
			t.Errorf("%s: %+v", r.Stage, r)
		}
	}
	if strings.Contains(body, key) {
		t.Fatalf("a secret reached the response: %s", body)
	}
}
