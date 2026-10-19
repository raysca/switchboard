package main

import (
	"context"
	"encoding/json"
	"fmt"
	"govoice/tools/internal/stack"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

func execOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// ServerOpts configures the child server.
type ServerOpts struct {
	Root        string // repo root (cwd of the server; model paths are relative to it)
	ServerBin   string // prebuilt server (vadworker next to it); "" builds one
	WorkDir     string
	VADMode     string // process | inprocess
	TurnManager bool
	MaxSessions int
	LLMDelay    time.Duration // providers.echo.llm_extra_delay: injected provider latency (the perf-gate self-test)
}

// Server is the child process under test. Stop is idempotent and kills the
// whole process group (server + vadworker children) so nothing is orphaned.
type Server struct {
	*stack.Server        // nil-child for ConnectTarget: Stop is then a no-op
	MetricsURL    string // metrics/probe base URL (default: Admin)
	ContainerName string // optional docker container name for resource stats
	mu            sync.Mutex
	cpuSeen       map[int]float64 // highest CPU time seen per pid, so restarted workers do not make CPU go backwards
}

func buildBinaries(root, dir string) (string, error) {
	for _, b := range [][]string{
		{"-o", filepath.Join(dir, "server"), "./cmd/server"},
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

func configYAML(o ServerOpts, pub, adm int) []byte {
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
  max_lifetime: 1m
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
providers:
  echo:
    mimic: instant
    stt: audio
    llm_extra_delay: %s
%s`, pub, adm, maxS, maxS, maxS, maxS, o.VADMode, o.LLMDelay, turn)
	return []byte(y)
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
	ss, err := stack.Start(ctx, stack.Spec{Root: o.Root, Bin: bin, Dir: o.WorkDir, Env: os.Environ(), Args: []string{"-vad-mode", o.VADMode},
		Config: func(pub, adm int) []byte { return configYAML(o, pub, adm) }})
	if err != nil {
		return nil, err
	}
	return &Server{Server: ss, cpuSeen: map[int]float64{}}, nil
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
		Server:        &stack.Server{Public: targetURL, Admin: adminURL, Key: apiKey},
		MetricsURL:    metricsURL,
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

// Provision creates the echo flow, returning its id.
func (s *Server) Provision() (string, error) {
	d, err := s.Call("POST", "flows", map[string]any{"name": "Loadtest Echo", "description": "echo provider", "config": map[string]any{
		"stt": map[string]any{"provider": "echo", "model": "chirp_2", "language": "en-US"},
		"llm": map[string]any{"provider": "echo", "model": "fake", "system_prompt": "fake"},
		"tts": map[string]any{"provider": "echo", "voice": "fake"}}})
	if err != nil {
		return "", err
	}
	var ag struct{ ID string }
	if err := json.Unmarshal(d, &ag); err != nil || ag.ID == "" {
		return "", fmt.Errorf("flow response: %v", err)
	}
	return ag.ID, nil
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
	if s.PID() == 0 {
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
