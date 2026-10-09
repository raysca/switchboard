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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func execOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// freePorts returns n distinct ephemeral ports (listeners held together so the
// OS cannot hand out the same one twice). ponytail: still a close-then-bind
// race with other processes; a lost race shows as "server exited during start".
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

// ServerOpts configures the child server.
type ServerOpts struct {
	Root        string // repo root (cwd of the server; model paths are relative to it)
	ServerBin   string // prebuilt fakeproviders server; "" builds one
	WorkDir     string
	VADMode     string // process | inprocess
	TurnManager bool
	MaxSessions int
}

// Server is the child process under test. Stop is idempotent and kills the
// whole process group (server + vadworker children) so nothing is orphaned.
type Server struct {
	cmd              *exec.Cmd
	PublicURL, Admin string
	MetricsURL       string // metrics/probe base URL (default: Admin)
	ContainerName    string // optional docker container name for resource stats
	key              string // management API key; never printed or logged
	LogPath          string
	done             chan struct{}
	mu               sync.Mutex
	cpuSeen          map[int]float64 // highest CPU time seen per pid, so restarted workers do not make CPU go backwards
}

func buildBinaries(root, dir string) (string, error) {
	for _, b := range [][]string{
		{"-tags", "fakeproviders", "-o", filepath.Join(dir, "server"), "./cmd/server"},
		{"-o", filepath.Join(dir, "vadworker"), "./cmd/vadworker"},
	} {
		c := exec.Command("go", append([]string{"build"}, b...)...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			return "", fmt.Errorf("go build %v: %v\n%s", b, err, out)
		}
	}
	return filepath.Join(dir, "server"), nil
}

func writeConfig(path string, pub, adm int, o ServerOpts) error {
	turn := ""
	if o.TurnManager {
		turn = "turn:\n  manager: true\n"
	}
	maxS := max(o.MaxSessions, 1000)
	y := fmt.Sprintf(`profile: development
public_listen_addr: "127.0.0.1:%d"
admin_listen_addr: "127.0.0.1:%d"
origin_allowlist: ["*"]
token:
  secret_env: "TOKEN_SECRET"
  issuer: "govoice-v1"
  audience: "govoice-v1-client"
  key_id: "v1"
  max_lifetime: 5m
  clock_skew: 30s
  replay_cache_limit: 100000
limits:
  max_pending_handshakes: %d
  max_sessions: %d
  max_sessions_per_principal: %d
  max_sessions_per_ip: %d
  upgrade_timeout: 10s
  ready_timeout: 20s
  max_call_duration: 4h
  idle_timeout: 60s
  drain_timeout: 10s
vad:
  mode: "%s"
  workers: 4
  batch: 8
  model_path: "third_party/silero_vad.onnx"
  queue_cap: 8192
observability:
  log_level: "warn"
  json_format: false
  timeline_max_events: 100
  timeline_ttl: 1m
storage:
  dialect: "sqlite"
retention:
  enabled: false
%s`, pub, adm, maxS, maxS, maxS, maxS, o.VADMode, turn)
	return os.WriteFile(path, []byte(y), 0o600)
}

func StartServer(ctx context.Context, o ServerOpts) (*Server, error) {
	bin := o.ServerBin
	if bin == "" {
		var err error
		if bin, err = buildBinaries(o.Root, o.WorkDir); err != nil {
			return nil, err
		}
	} else if _, err := os.Stat(filepath.Join(filepath.Dir(bin), "vadworker")); err != nil && o.VADMode == "process" {
		return nil, fmt.Errorf("vadworker must sit next to %s: %w", bin, err)
	}
	ports, err := freePorts(2)
	if err != nil {
		return nil, err
	}
	pub, adm := ports[0], ports[1]
	cfgPath := filepath.Join(o.WorkDir, "server.yaml")
	if err := writeConfig(cfgPath, pub, adm, o); err != nil {
		return nil, err
	}
	dev := filepath.Join(o.WorkDir, "data")
	if err := os.MkdirAll(dev, 0o700); err != nil {
		return nil, err
	}
	s := &Server{PublicURL: fmt.Sprintf("127.0.0.1:%d", pub), Admin: fmt.Sprintf("http://127.0.0.1:%d", adm), key: randHex(32),
		LogPath: filepath.Join(o.WorkDir, "server.log"), done: make(chan struct{}), cpuSeen: map[int]float64{}}
	logf, err := os.Create(s.LogPath)
	if err != nil {
		return nil, err
	}
	// ponytail: exec.Command, not CommandContext: cleanup must signal the group, not just the pid.
	s.cmd = exec.Command(bin, "-config", cfgPath, "-vad-mode", o.VADMode)
	s.cmd.Dir = o.Root
	s.cmd.Stdout, s.cmd.Stderr = logf, logf
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd.Env = append(os.Environ(), "GOVOICE_ROOT="+o.Root, "GOVOICE_DEV_DIR="+dev, "GOVOICE_MANAGEMENT_API_KEY="+s.key,
		"TOKEN_SECRET="+randHex(32), "GOVOICE_FAKE_STT=audio")
	if err := s.cmd.Start(); err != nil {
		return nil, err
	}
	go func() { _ = s.cmd.Wait(); logf.Close(); close(s.done) }()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		select {
		case <-s.done:
			return nil, fmt.Errorf("server exited during start (log: %s)", s.LogPath)
		case <-ctx.Done():
			s.Stop()
			return nil, ctx.Err()
		default:
		}
		if r, err := http.Get(s.Admin + "/ready"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return s, nil
			}
		}
	}
	s.Stop()
	return nil, fmt.Errorf("server not ready in 60s (log: %s)", s.LogPath)
}

// ConnectTarget connects to an existing or containerized server under test.
func ConnectTarget(ctx context.Context, targetURL, adminURL, metricsURL, apiKey, container string) (*Server, error) {
	if !strings.HasPrefix(adminURL, "http://") && !strings.HasPrefix(adminURL, "https://") {
		adminURL = "http://" + adminURL
	}
	if metricsURL != "" && !strings.HasPrefix(metricsURL, "http://") && !strings.HasPrefix(metricsURL, "https://") {
		metricsURL = "http://" + metricsURL
	}
	if metricsURL == "" {
		metricsURL = adminURL
	}
	targetURL = strings.TrimPrefix(targetURL, "ws://")
	targetURL = strings.TrimPrefix(targetURL, "http://")
	targetURL = strings.TrimSuffix(targetURL, "/ws")

	s := &Server{
		PublicURL:     targetURL,
		Admin:         adminURL,
		MetricsURL:    metricsURL,
		key:           apiKey,
		ContainerName: container,
		cpuSeen:       map[int]float64{},
	}

	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if r, err := client.Get(metricsURL + "/ready"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return s, nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("target server at %s (/ready) not ready after 60s", metricsURL)
}

func (s *Server) PID() int {
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

// Stop sends SIGTERM to the group, waits for graceful exit, then SIGKILLs the group.
func (s *Server) Stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	pgid := s.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL) // sweeps vadworker stragglers; ESRCH when none
	<-s.done
}

// api POSTs to the management API with the generated key.
func (s *Server) api(path string, body any) (json.RawMessage, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", s.Admin+"/api/v1/"+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("POST %s: %s", path, resp.Status) // body omitted: could echo secrets
	}
	var env struct{ Data json.RawMessage }
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// Provision creates the fake credential and agent, returning the agent id.
func (s *Server) Provision() (string, error) {
	d, err := s.api("providers/credentials", map[string]string{"provider": "google", "secret": "fake-loadtest-key"})
	if err != nil {
		return "", err
	}
	var cred struct{ ID string }
	if err := json.Unmarshal(d, &cred); err != nil || cred.ID == "" {
		return "", fmt.Errorf("credential response: %v", err)
	}
	d, err = s.api("agents", map[string]any{"name": "Loadtest Fake", "description": "fake providers", "config": map[string]any{
		"stt": map[string]any{"provider": "google", "model": "chirp_2", "language": "en-US", "credential_id": cred.ID},
		"llm": map[string]any{"provider": "google", "model": "fake", "system_prompt": "fake", "credential_id": cred.ID},
		"tts": map[string]any{"provider": "google", "voice": "fake", "credential_id": cred.ID}}})
	if err != nil {
		return "", err
	}
	var ag struct{ ID string }
	if err := json.Unmarshal(d, &ag); err != nil || ag.ID == "" {
		return "", fmt.Errorf("agent response: %v", err)
	}
	return ag.ID, nil
}

// Token mints a single-use voice token (one per call).
func (s *Server) Token(principal, agentID string) (string, error) {
	d, err := s.api("voice-tokens", map[string]string{"principal": principal, "agent_id": agentID})
	if err != nil {
		return "", err
	}
	var t struct{ Token string }
	if err := json.Unmarshal(d, &t); err != nil || t.Token == "" {
		return "", fmt.Errorf("token response: %v", err)
	}
	return t.Token, nil
}

// ---- /metrics ----

var promLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)$`)
var labelRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

type promSample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

func parseProm(text string) []promSample {
	var out []promSample
	for _, ln := range strings.Split(text, "\n") {
		m := promLine.FindStringSubmatch(strings.TrimSpace(ln))
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		ps := promSample{Name: m[1], Value: v, Labels: map[string]string{}}
		for _, l := range labelRe.FindAllStringSubmatch(m[2], -1) {
			ps.Labels[l[1]] = l[2]
		}
		out = append(out, ps)
	}
	return out
}

// Snapshot is one /metrics scrape.
type Snapshot struct {
	Gauges  map[string]float64             // unlabelled series
	Rejects map[string]float64             // admission rejects by reason
	Buckets map[string]map[float64]float64 // stage -> le -> cumulative count (+Inf = math.Inf)
	Sum     map[string]float64
	Count   map[string]float64
}

func (s *Server) Scrape() (*Snapshot, error) {
	baseURL := s.MetricsURL
	if baseURL == "" {
		baseURL = s.Admin
	}
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(baseURL + "/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return snapshotFrom(string(b)), nil
}

func snapshotFrom(text string) *Snapshot {
	sn := &Snapshot{Gauges: map[string]float64{}, Rejects: map[string]float64{}, Buckets: map[string]map[float64]float64{}, Sum: map[string]float64{}, Count: map[string]float64{}}
	for _, p := range parseProm(text) {
		stage := p.Labels["stage"]
		switch {
		case p.Name == "voice_admission_rejects_total":
			sn.Rejects[p.Labels["reason"]] = p.Value
		case p.Name == "voice_latency_seconds_bucket":
			le, err := strconv.ParseFloat(p.Labels["le"], 64) // "+Inf" parses
			if err != nil {
				continue
			}
			if sn.Buckets[stage] == nil {
				sn.Buckets[stage] = map[float64]float64{}
			}
			sn.Buckets[stage][le] = p.Value
		case p.Name == "voice_latency_seconds_sum":
			sn.Sum[stage] = p.Value
		case p.Name == "voice_latency_seconds_count":
			sn.Count[stage] = p.Value
		case len(p.Labels) == 0:
			sn.Gauges[p.Name] = p.Value
		}
	}
	return sn
}

// ---- process tree stats ----

// ProcStats is a point-in-time reading of the server and its descendants.
type ProcStats struct {
	RSSMB  float64
	CPUSec float64
	FDs    int
}

// cpuTime parses ps TIME: [[dd-]hh:]mm:ss[.ff].
func cpuTime(s string) float64 {
	s = strings.TrimSpace(s)
	days := 0.0
	if i := strings.Index(s, "-"); i >= 0 {
		days, _ = strconv.ParseFloat(s[:i], 64)
		s = s[i+1:]
	}
	total := 0.0
	for _, part := range strings.Split(s, ":") {
		v, _ := strconv.ParseFloat(part, 64)
		total = total*60 + v
	}
	return total + days*86400
}

func treePIDs(psOut string, root int) (pids []int, rss map[int]float64, cpu map[int]float64) {
	parent, rss, cpu := map[int]int{}, map[int]float64{}, map[int]float64{}
	for _, ln := range strings.Split(psOut, "\n") {
		f := strings.Fields(ln)
		if len(f) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		kb, _ := strconv.ParseFloat(f[2], 64)
		parent[pid], rss[pid], cpu[pid] = ppid, kb/1024, cpuTime(f[3])
	}
	in := map[int]bool{root: true}
	for changed := true; changed; { // fixed point over a small table
		changed = false
		for pid, pp := range parent {
			if !in[pid] && in[pp] {
				in[pid], changed = true, true
			}
		}
	}
	for pid := range in {
		if _, ok := rss[pid]; ok {
			pids = append(pids, pid)
		}
	}
	return
}

func (s *Server) Proc() ProcStats {
	if s.ContainerName != "" {
		out, err := execOutput("docker", "exec", s.ContainerName, "ps", "-axo", "pid=,ppid=,rss=,time=")
		if err == nil {
			pids, rss, cpu := treePIDs(out, 1)
			var st ProcStats
			s.mu.Lock()
			for _, p := range pids {
				st.RSSMB += rss[p]
				s.cpuSeen[p] = max(s.cpuSeen[p], cpu[p])
			}
			for _, c := range s.cpuSeen {
				st.CPUSec += c
			}
			s.mu.Unlock()
			if fdOut, err := execOutput("docker", "exec", s.ContainerName, "sh", "-c", "ls -1 /proc/[0-9]*/fd 2>/dev/null | wc -l"); err == nil {
				if fds, err := strconv.Atoi(strings.TrimSpace(fdOut)); err == nil {
					st.FDs = fds
				}
			}
			return st
		}
	}
	if s.cmd == nil || s.cmd.Process == nil {
		return ProcStats{}
	}
	out, err := execOutput("ps", "-axo", "pid=,ppid=,rss=,time=")
	if err != nil {
		return ProcStats{}
	}
	pids, rss, cpu := treePIDs(out, s.PID())
	var st ProcStats
	var list []string
	s.mu.Lock()
	for _, p := range pids {
		st.RSSMB += rss[p]
		s.cpuSeen[p] = max(s.cpuSeen[p], cpu[p])
		list = append(list, strconv.Itoa(p))
	}
	for _, c := range s.cpuSeen {
		st.CPUSec += c
	}
	s.mu.Unlock()
	// open fds across the tree. ponytail: lsof, ~100 ms at 5 s sampling; /proc on linux would be cheaper.
	if len(list) > 0 {
		if lo, err := execOutput("lsof", "-nP", "-p", strings.Join(list, ",")); err == nil {
			for _, ln := range strings.Split(lo, "\n") {
				if f := strings.Fields(ln); len(f) > 3 && f[3][0] >= '0' && f[3][0] <= '9' { // real descriptors, not txt/mem/cwd
					st.FDs++
				}
			}
		}
	}
	return st
}
