package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// mode is one turn configuration the scenarios run under (server-level, so one server each).
type mode struct {
	name, turn, extra string // extra: more top-level server.yaml (e.g. tts_merge)
	vadStopMS         int    // vad.stop_ms (0 = server default 800)
	extraLLM          int    // extra LLM requests in the estimate (eager can split a turn into a second one)
}

var modes = map[string]mode{
	"off":      {"off", "", "", 0, 0},
	"on":       {"on", "turn:\n  manager: true\n  min_words: 3\n", "", 0, 0},
	"on+smart": {"on+smart", "turn:\n  manager: true\n  min_words: 3\n  stop: [turn-analyzer, speech-timeout]\n", "", 0, 0},
	// slack-aware TTS merging (xai/google; docs/benchmarks/tts-unit-sizing.md): same server as "off" otherwise.
	// first_clause_chars 40 is what the server implies when merging is on and the key is 0.
	"merge": {"merge", "", "tts_merge: {enabled: true, margin: 300ms, cap: 250}\n", 0, 0},
	// first-audio latency fixes (docs/benchmarks/first-audio-latency.md, Fix measurements)
	"eager":        {"eager", "turn:\n  manager: true\n  stop: [eager, speech-timeout]\n  eager_grace: 200ms\n", "", 0, 1},
	"vad500":       {"vad500", "", "", 500, 0},
	"vad300+smart": {"vad300+smart", "turn:\n  manager: true\n  stop: [turn-analyzer, speech-timeout]\n", "", 300, 0},
	// kept from the first livecheck so old -variants lists still work
	"plain": {"plain", "turn:\n  manager: true\n", "", 0, 0},
	"fast":  {"fast", "turn:\n  manager: true\n  min_words: 3\n  speech_timeout: 150ms\n", "", 0, 0},
}

var modeAlias = map[string]string{"smart": "on+smart"}

// serverClass is what a scenario needs from the server beyond the mode: S5 needs short idle
// timers and S8 short ping settings, so those run on their own server.
type serverClass struct{ name, yaml string }

var (
	classBase = serverClass{"base", ""}
	classIdle = serverClass{"idle", "idle: {user_timeout: 4s, max_prompts: 1, prompt: \"Are you still there?\", prompt_mode: say, goodbye: \"Goodbye.\"}\n"}
	// idle_timeout is the legacy data-silence deadline; with ping_interval set it is not used,
	// which is what S8 proves: a muted client outlives idle_timeout.
	classPing = serverClass{"ping", ""}
	// classFailover runs S13 in its own server: its broken primary key must not open the
	// process-wide breaker for the other scenarios.
	classFailover = serverClass{"failover", ""}
	// classGraph runs S14 in its own server: its agent is a two-node graph.
	classGraph = serverClass{"graph", ""}
)

const pingIdleTimeout = 3 * time.Second

type server struct {
	cmd           *exec.Cmd
	public, admin string
	key           string
	log           string
	data          string // credential store; removed at stop, even with -keep-work
	done          chan struct{}
	calls         int
	lastReq       atomic.Int64 // last good requestTotal
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func freePorts(n int) ([]int, error) {
	var ports []int
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		defer l.Close()
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// buildServer builds the server child with tags plus the vad/turn workers into dir.
func buildServer(root, dir string, tags []string) (string, error) {
	builds := [][]string{{"-o", filepath.Join(dir, "vadworker"), "./cmd/vadworker"}, {"-o", filepath.Join(dir, "turnworker"), "./cmd/turnworker"}}
	srv := []string{"-o", filepath.Join(dir, "server"), "./cmd/server"}
	if len(tags) > 0 {
		srv = append([]string{"-tags", strings.Join(tags, ",")}, srv...)
	}
	for _, b := range append([][]string{srv}, builds...) {
		c := exec.Command("go", append([]string{"build"}, b...)...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			return "", fmt.Errorf("go build %v: %v\n%s", b, err, out)
		}
	}
	return filepath.Join(dir, "server"), nil
}

type serverSpec struct {
	root, bin, work string
	name            string // unique dir name under work
	mode            mode
	class           serverClass
	infra           map[string]map[string]string // providers.<name>.<key>
	fake            bool
	logLevel        string
}

// serverYAML is the server config for spec on the given ports.
func serverYAML(sp serverSpec, pub, adm int) string {
	limits := "limits: {max_sessions: 10, max_sessions_per_principal: 10, upgrade_timeout: 10s, ready_timeout: 20s, max_call_duration: 10m, idle_timeout: 60s, drain_timeout: 10s}"
	if sp.class.name == "ping" {
		limits = fmt.Sprintf("limits: {max_sessions: 10, max_sessions_per_principal: 10, upgrade_timeout: 10s, ready_timeout: 20s, max_call_duration: 10m, idle_timeout: %s, ping_interval: 1s, ping_timeout: 500ms, drain_timeout: 10s}", pingIdleTimeout)
	}
	infra := map[string]map[string]string{"google_tts": {"sample_rate": "24000"}}
	for k, m := range sp.infra {
		if infra[k] == nil {
			infra[k] = map[string]string{}
		}
		for kk, v := range m {
			infra[k][kk] = v
		}
	}
	var pv strings.Builder
	names := sortedKeys(infra)
	for _, n := range names {
		var kv []string
		for _, k := range sortedKeys(infra[n]) {
			v := infra[n][k]
			if k != "sample_rate" {
				v = fmt.Sprintf("%q", v)
			}
			kv = append(kv, k+": "+v)
		}
		pv.WriteString(fmt.Sprintf("  %s: {%s}\n", n, strings.Join(kv, ", ")))
	}
	vadStop := ""
	if ms := sp.mode.vadStopMS; ms > 0 {
		vadStop = fmt.Sprintf(", stop_ms: %d", ms)
	}
	return fmt.Sprintf(`profile: development
public_listen_addr: "127.0.0.1:%d"
admin_listen_addr: "127.0.0.1:%d"
origin_allowlist: ["*"]
token: {secret_env: "TOKEN_SECRET", issuer: "govoice-v1", audience: "govoice-v1-client", key_id: "v1", max_lifetime: 5m, clock_skew: 30s, replay_cache_limit: 1000}
%s
vad: {mode: "process", workers: 1, batch: 8, model_path: "third_party/silero_vad.onnx", queue_cap: 8192%s}
providers:
  google_stt: {project_id: "", location: "us-central1", rotation_threshold: 240s}
%sobservability: {log_level: "%s", json_format: false, timeline_max_events: 500, timeline_ttl: 10m}
storage: {dialect: "sqlite"}
retention: {enabled: false}
%s%s%s
`, pub, adm, limits, vadStop, pv.String(), sp.logLevel, sp.mode.turn, sp.mode.extra, sp.class.yaml)
}

func sortedKeys[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// childEnvAllow is everything the server child inherits. An allowlist, not a denylist: provider
// keys (the child must use stored credentials only), unrelated tokens, and GOVOICE_DATABASE_DSN /
// GOVOICE_CREDENTIAL_KEYS (they would point the child at a real store that outlives the run) stay out.
var childEnvAllow = []string{"PATH", "HOME", "TMPDIR", "USER", "LOGNAME", "LANG", "LC_ALL", "TZ",
	"DYLD_LIBRARY_PATH", "LD_LIBRARY_PATH", "ORT_DISABLE_TELEMETRY", "GOVOICE_ORT_LIB", "GOVOICE_MODEL_PATH",
	"GOVOICE_FAKE_LLM_DELAY"} // read only by -tags fakeproviders: emulates provider latency in the self-test

func childEnv(environ []string) []string {
	var env []string
	for _, e := range environ {
		if k, _, _ := strings.Cut(e, "="); slices.Contains(childEnvAllow, k) {
			env = append(env, e)
		}
	}
	return env
}

func startServer(ctx context.Context, sp serverSpec) (*server, error) {
	ports, err := freePorts(2)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(sp.work, sp.name)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(cfgPath, []byte(serverYAML(sp, ports[0], ports[1])), 0o600); err != nil {
		return nil, err
	}
	s := &server{public: fmt.Sprintf("127.0.0.1:%d", ports[0]), admin: fmt.Sprintf("http://127.0.0.1:%d", ports[1]),
		key: randHex(32), log: filepath.Join(dir, "server.log"), data: filepath.Join(dir, "data"), done: make(chan struct{})}
	theRedactor.add(s.key)
	logf, err := os.Create(s.log)
	if err != nil {
		return nil, err
	}
	s.cmd = exec.Command(sp.bin, "-config", cfgPath)
	s.cmd.Dir = sp.root
	s.cmd.Stdout, s.cmd.Stderr = logf, logf
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd.Env = append(childEnv(os.Environ()), "GOVOICE_ROOT="+sp.root, "GOVOICE_DEV_DIR="+s.data,
		"GOVOICE_MANAGEMENT_API_KEY="+s.key, "TOKEN_SECRET="+randHex(32))
	if sp.fake {
		s.cmd.Env = append(s.cmd.Env, "GOVOICE_FAKE_STT=audio")
	}
	if err := s.cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	go func() { _ = s.cmd.Wait(); logf.Close(); close(s.done) }()
	for dl := time.Now().Add(60 * time.Second); time.Now().Before(dl); time.Sleep(200 * time.Millisecond) {
		select {
		case <-s.done:
			return nil, fmt.Errorf("server exited during start (log: %s)", s.log)
		case <-ctx.Done():
			s.stop()
			return nil, ctx.Err()
		default:
		}
		if r, err := http.Get(s.admin + "/ready"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return s, nil
			}
		}
	}
	s.stop()
	return nil, fmt.Errorf("server not ready in 60s (log: %s)", s.log)
}

// stop terminates the process group (server and its workers) and deletes the credential store.
func (s *server) stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	pg := s.cmd.Process.Pid
	_ = syscall.Kill(-pg, syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
	}
	_ = syscall.Kill(-pg, syscall.SIGKILL)
	<-s.done
	_ = os.RemoveAll(s.data) // stored provider credentials never outlive the run
}

// call is one management API request. Response bodies are never echoed: they could contain secrets.
func (s *server) call(method, path string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.admin+"/api/v1/"+path, rd)
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	var env struct{ Data json.RawMessage }
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

func (s *server) api(path string, body any) (json.RawMessage, error) {
	return s.call("POST", path, body)
}

func (s *server) id(path string, body any) (string, error) {
	d, err := s.api(path, body)
	if err != nil {
		return "", err
	}
	var r struct{ ID string }
	if err := json.Unmarshal(d, &r); err != nil || r.ID == "" {
		return "", fmt.Errorf("%s: no id in response", path)
	}
	return r.ID, nil
}

func (s *server) token(agent string) (string, error) {
	d, err := s.api("voice-tokens", map[string]string{"principal": "livecheck", "agent_id": agent})
	if err != nil {
		return "", err
	}
	var t struct{ Token string }
	if err := json.Unmarshal(d, &t); err != nil || t.Token == "" {
		return "", fmt.Errorf("token response: %v", err)
	}
	return t.Token, nil
}

// logProblems returns the redacted WARN/ERROR lines of the server log.
func (s *server) logProblems() []string {
	b, _ := os.ReadFile(s.log)
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if benignLog(ln) {
			continue
		}
		if strings.Contains(ln, "ERROR") || strings.Contains(ln, "WARN") || strings.Contains(ln, "level=error") || strings.Contains(ln, "level=warn") {
			out = append(out, truncate(theRedactor.String(ln), 300))
		}
	}
	return out
}

// stageStat is the running sum/count of one voice_latency_seconds stage.
type stageStat struct{ sum, n float64 }

var latencyRe = regexp.MustCompile(`^voice_latency_seconds_(sum|count)\{[^}]*stage="([^"]+)"[^}]*\}\s+(\S+)`)

// stages scrapes /metrics: cumulative latency per stage.
func (s *server) stages() map[string]stageStat {
	out := map[string]stageStat{}
	resp, err := http.Get(s.admin + "/metrics")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, ln := range strings.Split(string(b), "\n") {
		if m := latencyRe.FindStringSubmatch(ln); m != nil {
			var v float64
			fmt.Sscan(m[3], &v)
			st := out[m[2]]
			if m[1] == "sum" {
				st.sum = v
			} else {
				st.n = v
			}
			out[m[2]] = st
		}
	}
	return out
}

var reqRe = regexp.MustCompile(`^voice_provider_requests_total\{provider="([^"]+)",stage="([^"]+)"\}\s+(\d+)`)

// requests scrapes /metrics: cumulative provider requests per "provider/stage" measured at the
// provider boundary (HTTP calls, WebSocket dials, gRPC opens), retries included. nil when unreachable.
func (s *server) requests() map[string]int {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(s.admin + "/metrics")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	out := map[string]int{}
	for _, ln := range strings.Split(string(b), "\n") {
		if m := reqRe.FindStringSubmatch(ln); m != nil {
			n, _ := strconv.Atoi(m[3])
			out[m[1]+"/"+m[2]] = n
		}
	}
	return out
}

var failoverRe = regexp.MustCompile(`^voice_llm_failover_total\{[^}]*\} (\d+)`)

// failovers is the server's total LLM failovers (voice_llm_failover_total), 0 if unreadable.
func (s *server) failovers() int {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(s.admin + "/metrics")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	n := 0
	for _, ln := range strings.Split(string(b), "\n") {
		if m := failoverRe.FindStringSubmatch(ln); m != nil {
			v, _ := strconv.Atoi(m[1])
			n += v
		}
	}
	return n
}

// requestTotal is the sum of requests(); ok is false (and n the last good value) when the scrape
// fails, so callers can fail closed: an unreadable counter must never mean "unlimited".
func (s *server) requestTotal() (n int, ok bool) {
	m := s.requests()
	if m == nil {
		return int(s.lastReq.Load()), false
	}
	for _, v := range m {
		n += v
	}
	s.lastReq.Store(int64(n))
	return n, true
}

// stageDelta is the mean ms and count of each stage observed between two scrapes.
func stageDelta(before, after map[string]stageStat) map[string][2]float64 {
	out := map[string][2]float64{}
	for k, a := range after {
		b := before[k]
		if n := a.n - b.n; n > 0 {
			out[k] = [2]float64{(a.sum - b.sum) / n * 1000, n}
		}
	}
	return out
}

type tlEvent struct {
	Type, Details string
	Timestamp     time.Time
}

// timelineEvents fetches the server-side session timeline (admin /debug/timeline) for session n.
func (s *server) timelineEvents(n int) []tlEvent {
	resp, err := http.Get(fmt.Sprintf("%s/debug/timeline?session=%d", s.admin, n))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var tl struct{ Events []tlEvent }
	if json.NewDecoder(resp.Body).Decode(&tl) != nil {
		return nil
	}
	return tl.Events
}

// timeline prints the server-side session timeline for session n.
func (s *server) timeline(n int) {
	tl := s.timelineEvents(n)
	if len(tl) == 0 {
		return
	}
	t0 := tl[0].Timestamp
	for _, e := range tl {
		fmt.Fprintf(stdout, "    srv %7.2fs %-14s %s\n", e.Timestamp.Sub(t0).Seconds(), e.Type, truncate(e.Details, 120))
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// benignLog matches the WARN lines every harness server prints at start.
func benignLog(ln string) bool {
	for _, b := range []string{"FAKE PROVIDERS", "development profile: using a local agent store", "retention disabled"} {
		if strings.Contains(ln, b) {
			return true
		}
	}
	return false
}
