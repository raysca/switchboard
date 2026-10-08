// livecheck drives a REAL server with REAL Google providers (spends API credit) through
// scripted turn scenarios over the actual WebSocket/RTVI protocol and prints measured
// voice-to-voice latency, transcripts and barge-in/pause behavior for the default
// VAD-driven turns vs the turn manager (turn.manager) with and without Smart-turn.
//
// Opt-in: `GOVOICE_LIVE=1 make live-check` (needs examples/.env, see examples/.env.example).
// Secrets are read from the env file, stored through the management API like `make dev`
// does, and never printed; server log lines are redacted before they are shown.
// Results are small-n timings, not benchmarks: see docs/benchmarks/turn-manager-live.md.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/examples/envload"
	"govoice/providers/google"
	"govoice/providers/nvidia"
	"govoice/providers/nvidia/rivapb"
	"govoice/transport/ws"
)

const (
	rate       = 16000
	chunkMS    = 20
	chunkBytes = rate * chunkMS / 1000 * 2
)

var silenceChunk = make([]byte, chunkBytes)

func pcmDur(b []byte) time.Duration     { return time.Duration(len(b)/2) * time.Second / rate }
func pcmSilence(d time.Duration) []byte { return make([]byte, int(d*rate/time.Second)*2) }

// ---- clips ----

// loadWAV returns the PCM16 mono 16 kHz data of a WAV with silence trimmed (100 ms margin).
func loadWAV(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	i := bytes.Index(b, []byte("data"))
	if len(b) < 12 || string(b[:4]) != "RIFF" || i < 0 {
		return nil, fmt.Errorf("%s: not a WAV", path)
	}
	pcm := b[i+8:]
	loud := func(f int) bool {
		var s float64
		for j := f * chunkBytes; j < (f+1)*chunkBytes && j+1 < len(pcm); j += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(pcm[j:])))
			s += v * v
		}
		return math.Sqrt(s/(chunkBytes/2)) > 50
	}
	first, last := -1, -1
	for f := 0; f < len(pcm)/chunkBytes; f++ {
		if loud(f) {
			if first < 0 {
				first = f
			}
			last = f
		}
	}
	if first < 0 {
		return nil, fmt.Errorf("%s: no speech", path)
	}
	a, z := max(first-5, 0)*chunkBytes, min(last+6, len(pcm)/chunkBytes)*chunkBytes
	return pcm[a:z], nil
}

// genClips synthesizes the scenario clips with Google TTS (4 calls) unless cached in dir.
func genClips(ctx context.Context, dir string, sa []byte, apiKey string) (map[string][]byte, error) {
	texts := map[string]string{
		"mars":   "Tell me about the planet Mars.",
		"uhhuh":  "Uh-huh, yeah.",
		"pauseA": "I would like to know",
		"pauseB": "what the capital of France is.",
	}
	out := map[string][]byte{}
	var cli *google.RealGoogleTTSClient
	for k, t := range texts {
		p := filepath.Join(dir, k+".pcm")
		if b, err := os.ReadFile(p); err == nil {
			out[k] = b
			continue
		}
		if cli == nil {
			cfg := google.DefaultTTSConfig()
			cfg.CredentialsJSON, cfg.APIKey = sa, apiKey
			if len(sa) > 0 {
				cfg.APIKey = ""
			}
			var err error
			if cli, err = google.NewRealGoogleTTSClient(ctx, cfg); err != nil {
				return nil, fmt.Errorf("tts client: %v", err)
			}
			defer cli.Close()
		}
		pcm, err := cli.SynthesizeSpeech(ctx, t, "en-US-Neural2-D", rate)
		if err != nil {
			return nil, fmt.Errorf("tts %s: %v", k, err)
		}
		out[k] = pcm
		if err := os.WriteFile(p, pcm, 0o600); err != nil {
			return nil, err
		}
		fmt.Printf("generated clip %s (%.1fs)\n", k, pcmDur(pcm).Seconds())
	}
	return out, nil
}

// genClipsNvidia is genClips on hosted Riva TTS (4 calls); the clips are the same texts at 16 kHz.
func genClipsNvidia(ctx context.Context, dir string, _ []byte, _ string) (map[string][]byte, error) {
	texts := map[string]string{
		"mars":   "Tell me about the planet Mars.",
		"uhhuh":  "Uh-huh, yeah.",
		"pauseA": "I would like to know",
		"pauseB": "what the capital of France is.",
	}
	out := map[string][]byte{}
	var conn *nvidia.Conn
	for k, t := range texts {
		p := filepath.Join(dir, k+".pcm")
		if b, err := os.ReadFile(p); err == nil {
			out[k] = b
			continue
		}
		if conn == nil {
			var err error
			if conn, err = nvidia.Dial(nvidia.ConnConfig{APIKey: os.Getenv("NVIDIA_API_KEY"), FunctionID: os.Getenv("NVIDIA_TTS_FUNCTION_ID")}); err != nil {
				return nil, err
			}
			defer conn.Close()
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		st, err := rivapb.NewRivaSpeechSynthesisClient(conn.Channel()).SynthesizeOnline(conn.Context(cctx))
		if err == nil {
			err = st.Send(&rivapb.SynthesizeSpeechRequest{Text: t, LanguageCode: "en-US", Encoding: rivapb.AudioEncoding_LINEAR_PCM,
				SampleRateHz: rate, VoiceName: "Magpie-Multilingual.EN-US.Aria"})
		}
		if err != nil {
			cancel()
			return nil, fmt.Errorf("tts %s: %v", k, err)
		}
		_ = st.CloseSend()
		var pcm []byte
		for {
			r, err := st.Recv()
			if err != nil {
				if err != io.EOF {
					cancel()
					return nil, fmt.Errorf("tts %s: %v", k, err)
				}
				break
			}
			pcm = append(pcm, r.GetAudio()...)
		}
		cancel()
		out[k] = pcm
		if err := os.WriteFile(p, pcm, 0o600); err != nil {
			return nil, err
		}
		fmt.Printf("generated clip %s (%.1fs)\n", k, pcmDur(pcm).Seconds())
	}
	return out, nil
}

// ---- server ----

type variant struct {
	name string
	turn string // yaml fragment
	idle string
}

type server struct {
	cmd           *exec.Cmd
	public, admin string
	key           string
	log           string
	data          string // credential store; removed at stop, even with -keep-work
	done          chan struct{}
	calls         int
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

func startServer(ctx context.Context, root, bin, work string, v variant) (*server, error) {
	ports, err := freePorts(2)
	if err != nil {
		return nil, err
	}
	cfg := fmt.Sprintf(`profile: development
public_listen_addr: "127.0.0.1:%d"
admin_listen_addr: "127.0.0.1:%d"
origin_allowlist: ["*"]
token: {secret_env: "TOKEN_SECRET", issuer: "govoice-v1", audience: "govoice-v1-client", key_id: "v1", max_lifetime: 5m, clock_skew: 30s, replay_cache_limit: 1000}
limits: {max_sessions: 10, max_sessions_per_principal: 10, upgrade_timeout: 10s, ready_timeout: 20s, max_call_duration: 10m, idle_timeout: 60s, drain_timeout: 10s}
vad: {mode: "process", workers: 1, batch: 8, model_path: "third_party/silero_vad.onnx", queue_cap: 8192}
providers:
  google_stt: {project_id: "", location: "us-central1", rotation_threshold: 240s}
  google_tts: {sample_rate: 24000}
observability: {log_level: "%s", json_format: false, timeline_max_events: 500, timeline_ttl: 10m}
storage: {dialect: "sqlite"}
retention: {enabled: false}
%s
%s
`, ports[0], ports[1], logLevel, v.turn, v.idle)
	dir := filepath.Join(work, v.name)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return nil, err
	}
	s := &server{public: fmt.Sprintf("127.0.0.1:%d", ports[0]), admin: fmt.Sprintf("http://127.0.0.1:%d", ports[1]),
		key: randHex(32), log: filepath.Join(dir, "server.log"), data: filepath.Join(dir, "data"), done: make(chan struct{})}
	logf, err := os.Create(s.log)
	if err != nil {
		return nil, err
	}
	s.cmd = exec.Command(bin, "-config", cfgPath)
	s.cmd.Dir = root
	s.cmd.Stdout, s.cmd.Stderr = logf, logf
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The child must use the stored credentials only: drop the Google env the harness loaded.
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GOOGLE_") && !strings.HasPrefix(e, "XAI_") && !strings.HasPrefix(e, "NVIDIA_") {
			env = append(env, e)
		}
	}
	s.cmd.Env = append(env, "GOVOICE_ROOT="+root, "GOVOICE_DEV_DIR="+filepath.Join(dir, "data"),
		"GOVOICE_MANAGEMENT_API_KEY="+s.key, "TOKEN_SECRET="+randHex(32))
	if err := s.cmd.Start(); err != nil {
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

func (s *server) api(path string, body any) (json.RawMessage, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", s.admin+"/api/v1/"+path, bytes.NewReader(b))
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

const prompt = "You are a voice assistant. Answer in one or two short sentences, except when asked to tell about a topic: then give a detailed answer of at least six sentences."

// provision stores the credentials and creates the agent exactly like scripts/dev-seed.mjs.
func (s *server) provision(sa []byte, apiKey string) (string, error) {
	if nv {
		return s.provisionNvidia()
	}
	speechSecret := apiKey
	if len(sa) > 0 {
		speechSecret = string(sa)
	}
	speech, err := s.id("providers/credentials", map[string]string{"provider": "google", "secret": speechSecret})
	if err != nil {
		return "", err
	}
	llm := speech
	if apiKey != "" {
		if llm, err = s.id("providers/credentials", map[string]string{"provider": "google", "secret": apiKey}); err != nil {
			return "", err
		}
	}
	return s.id("agents", map[string]any{"name": "Live Check", "description": "livecheck", "config": map[string]any{
		"stt": map[string]any{"provider": "google", "model": sttModel, "language": "en-US", "credential_id": speech},
		"llm": map[string]any{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": prompt, "credential_id": llm},
		"tts": map[string]any{"provider": "google", "voice": "en-US-Journey-F", "credential_id": speech}}})
}

// provisionNvidia stores one bare-key LLM credential and one STT/TTS credential (separate hosted
// Riva functions) and creates an all-nvidia agent. The values come from the harness env only.
func (s *server) provisionNvidia() (string, error) {
	key := os.Getenv("NVIDIA_API_KEY")
	riva, _ := json.Marshal(map[string]string{"api_key": key,
		"function_id": os.Getenv("NVIDIA_STT_FUNCTION_ID"), "tts_function_id": os.Getenv("NVIDIA_TTS_FUNCTION_ID")})
	speech, err := s.id("providers/credentials", map[string]string{"provider": "nvidia", "secret": string(riva)})
	if err != nil {
		return "", err
	}
	llm, err := s.id("providers/credentials", map[string]string{"provider": "nvidia", "secret": key})
	if err != nil {
		return "", err
	}
	return s.id("agents", map[string]any{"name": "Live Check", "description": "livecheck", "config": map[string]any{
		"stt": map[string]any{"provider": "nvidia", "language": "en-US", "credential_id": speech},
		"llm": map[string]any{"provider": "nvidia", "model": nimModel(), "system_prompt": prompt, "credential_id": llm},
		"tts": map[string]any{"provider": "nvidia", "credential_id": speech}}})
}

func nimModel() string {
	if m := os.Getenv("NVIDIA_LLM_MODEL"); m != "" {
		return m
	}
	return "openai/gpt-oss-20b" // server/nvidia.go nvidiaNIMModel
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

// redact is err's text with credential-looking substrings removed.
func redact(err error) string { return secretRe.ReplaceAllString(fmt.Sprint(err), "[redacted]") }

var secretRe = regexp.MustCompile(`AIza[\w-]{10,}|nvapi-[\w-]+|ya29\.[\w.-]+|-----BEGIN[^"\n]*|Bearer\s+\S+|[\w.-]+@[\w.-]*gserviceaccount\.com|eyJ[\w-]{20,}\.[\w.-]+|(?i:token|key|secret)=\S+`)

// logProblems returns the redacted WARN/ERROR lines of the server log.
func (s *server) logProblems() []string {
	b, _ := os.ReadFile(s.log)
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.Contains(ln, "ERROR") || strings.Contains(ln, "WARN") || strings.Contains(ln, "level=error") || strings.Contains(ln, "level=warn") {
			ln = secretRe.ReplaceAllString(ln, "[redacted]")
			out = append(out, truncate(ln, 300))
		}
	}
	return out
}

// stageMeans scrapes /metrics and returns "stage=meanMs(n)" for every voice_latency_seconds stage.
func (s *server) stageMeans() string {
	resp, err := http.Get(s.admin + "/metrics")
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	sum, cnt := map[string]float64{}, map[string]float64{}
	re := regexp.MustCompile(`^voice_latency_seconds_(sum|count)\{[^}]*stage="([^"]+)"[^}]*\}\s+(\S+)`)
	for _, ln := range strings.Split(string(b), "\n") {
		if m := re.FindStringSubmatch(ln); m != nil {
			var v float64
			fmt.Sscan(m[3], &v)
			if m[1] == "sum" {
				sum[m[2]] += v
			} else {
				cnt[m[2]] += v
			}
		}
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") { // smart-turn verdicts, fallbacks, inference count
		if (strings.HasPrefix(ln, "voice_smartturn_verdicts_total") || strings.HasPrefix(ln, "voice_smartturn_fallbacks_total") && !strings.HasSuffix(ln, " 0") || strings.HasPrefix(ln, "voice_smartturn_inference_ms_count") || strings.HasPrefix(ln, "voice_smartturn_inference_ms_sum")) && !strings.HasSuffix(ln, " 0") {
			out = append(out, strings.TrimPrefix(ln, "voice_smartturn_"))
		}
	}
	for st, n := range cnt {
		if n > 0 {
			out = append(out, fmt.Sprintf("%s=%.0fms(n=%.0f)", st, sum[st]/n*1000, n))
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// timeline prints the server-side session timeline (admin /debug/timeline) for session n.
func (s *server) timeline(n int) {
	resp, err := http.Get(fmt.Sprintf("%s/debug/timeline?session=%d", s.admin, n))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var tl struct {
		Events []struct {
			Type, Details string
			Timestamp     time.Time
		}
	}
	if json.NewDecoder(resp.Body).Decode(&tl) != nil || len(tl.Events) == 0 {
		return
	}
	t0 := tl.Events[0].Timestamp
	for _, e := range tl.Events {
		fmt.Printf("    srv %7.2fs %-14s %s\n", e.Timestamp.Sub(t0).Seconds(), e.Type, secretRe.ReplaceAllString(truncate(e.Details, 120), "[redacted]"))
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ---- client ----

type ev struct {
	t     time.Time
	kind  string // audio | RTVI type
	text  string
	final bool
}

type client struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	evs    []ev
	next   time.Time
	closed chan error
	t0     time.Time // bot-ready
	n      int       // server session number (1-based dial order)
}

func dial(ctx context.Context, s *server, agent string) (*client, error) {
	tok, err := s.token(agent)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, "ws://"+s.public+"/ws?token="+tok, nil)
	if err != nil {
		return nil, fmt.Errorf("dial: %v", err)
	}
	conn.SetReadLimit(1 << 20)
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err := conn.Write(dctx, websocket.MessageBinary, ready); err != nil {
		return nil, err
	}
	s.calls++
	c := &client{conn: conn, closed: make(chan error, 1), n: s.calls}
	for {
		_, data, err := conn.Read(dctx)
		if err != nil {
			return nil, fmt.Errorf("no bot-ready: %v", err)
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			if m, ok := f.(*ws.WireMessageFrame); ok && strings.Contains(m.Data, "bot-ready") {
				break
			}
		}
	}
	c.t0 = time.Now()
	go c.reader(context.Background())
	c.next = time.Now()
	return c, nil
}

func (c *client) reader(ctx context.Context) {
	seq := uint64(1)
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			c.closed <- err
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		seq++
		f, err := ws.DecodeBinaryFrame(data, seq, time.Now())
		if err != nil {
			continue
		}
		e := ev{t: time.Now()}
		switch fr := f.(type) {
		case *core.InputAudioFrame: // bot audio as seen by the client
			e.kind = "audio"
		case *ws.WireMessageFrame:
			var m struct {
				Type string
				Data struct {
					Text  string
					Final bool
					Error string
				}
			}
			if json.Unmarshal([]byte(fr.Data), &m) != nil {
				continue
			}
			e.kind, e.text, e.final = m.Type, m.Data.Text, m.Data.Final
			if m.Type == "error" {
				e.text = m.Data.Error // category only
			}
		default:
			continue
		}
		c.mu.Lock()
		c.evs = append(c.evs, e)
		c.mu.Unlock()
	}
}

func (c *client) snap() []ev {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ev(nil), c.evs...)
}

func (c *client) pace(b []byte) error {
	if d := time.Until(c.next); d > 0 {
		time.Sleep(d)
	}
	c.next = c.next.Add(chunkMS * time.Millisecond)
	f, _ := ws.EncodeAudioFrame(b, rate, 1)
	return c.conn.Write(context.Background(), websocket.MessageBinary, f)
}

// stream sends PCM in real time; the caller gets the wall-clock start and end.
func (c *client) stream(pcm []byte) (start, end time.Time, err error) {
	start = time.Now()
	for off := 0; off < len(pcm); off += chunkBytes {
		ch := pcm[off:min(off+chunkBytes, len(pcm))]
		if len(ch) < chunkBytes {
			ch = append(append([]byte(nil), ch...), silenceChunk[:chunkBytes-len(ch)]...)
		}
		if err = c.pace(ch); err != nil {
			return
		}
	}
	return start, time.Now(), nil
}

// wait streams silence until cond is true or limit passes; reports whether cond held.
func (c *client) wait(limit time.Duration, cond func() bool) bool {
	for end := time.Now().Add(limit); time.Now().Before(end); {
		if cond != nil && cond() {
			return true
		}
		select {
		case <-c.closed:
			return false
		default:
		}
		if c.pace(silenceChunk) != nil {
			return false
		}
	}
	return cond != nil && cond()
}

func (c *client) quiet(d time.Duration) { c.wait(d, nil) }

func (c *client) firstAfter(t time.Time, kinds ...string) (ev, bool) {
	for _, e := range c.snap() {
		if e.t.After(t) {
			for _, k := range kinds {
				if e.kind == k {
					return e, true
				}
			}
		}
	}
	return ev{}, false
}

func (c *client) has(t time.Time, kind string) bool { _, ok := c.firstAfter(t, kind); return ok }

// texts joins event texts of a kind after t ("final" restricts transcripts to finals).
func (c *client) texts(t time.Time, kind string, finalOnly bool) string {
	var parts []string
	for _, e := range c.snap() {
		if e.t.After(t) && e.kind == kind && (!finalOnly || e.final) {
			parts = append(parts, strings.TrimSpace(e.text))
		}
	}
	return strings.Join(parts, "")
}

func (c *client) count(t time.Time, kind string) (n int) {
	for _, e := range c.snap() {
		if e.t.After(t) && e.kind == kind {
			n++
		}
	}
	return
}

// audioRunEnd returns the end of the contiguous bot audio that covers `from`: the first
// frame after from followed by a gap >= gap. ok=false when audio never paused.
func (c *client) audioRunEnd(from time.Time, gap time.Duration) (time.Time, bool) {
	var last time.Time
	for _, e := range c.snap() {
		if e.kind != "audio" || e.t.Before(from) {
			continue
		}
		if !last.IsZero() && e.t.Sub(last) >= gap {
			return last, true
		}
		last = e.t
	}
	if !last.IsZero() && time.Since(last) >= gap { // audio ended and nothing followed
		return last, true
	}
	return last, false
}

func (c *client) audioNow(within time.Duration) bool {
	ev := c.snap()
	for i := len(ev) - 1; i >= 0; i-- {
		if ev[i].kind == "audio" {
			return time.Since(ev[i].t) < within
		}
	}
	return false
}

func (c *client) close() {
	for i := 0; i < 100 && c.audioNow(300*time.Millisecond); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

// timeline prints every non-audio event and bot audio runs (gap >= 250 ms starts a new run),
// relative to bot-ready. Transcript text is shown; error events carry a category only.
func (c *client) timeline() {
	var runStart, runEnd time.Time
	n := 0
	flush := func() {
		if n > 0 {
			fmt.Printf("    %7.2fs audio run %d frames until %.2fs\n", runStart.Sub(c.t0).Seconds(), n, runEnd.Sub(c.t0).Seconds())
			n = 0
		}
	}
	for _, e := range c.snap() {
		if e.kind == "audio" {
			if n > 0 && e.t.Sub(runEnd) >= 250*time.Millisecond {
				flush()
			}
			if n == 0 {
				runStart = e.t
			}
			n++
			runEnd = e.t
			continue
		}
		flush()
		fmt.Printf("    %7.2fs %s %q final=%v\n", e.t.Sub(c.t0).Seconds(), e.kind, truncate(e.text, 60), e.final)
	}
	flush()
}

var trace, probe, keep bool
var pauseLen time.Duration
var sttModel, logLevel string
var nv bool // -providers nvidia

func ms(d time.Duration) string { return fmt.Sprintf("%d", d.Milliseconds()) }

// ---- scenarios ----

type clips struct{ q, mars, uhhuh, pause []byte }

type result struct {
	scenario, variant string
	metrics           []string
}

func (r *result) add(k string, v any) { r.metrics = append(r.metrics, fmt.Sprintf("%s=%v", k, v)) }

func newResult(sc, v string) *result { return &result{scenario: sc, variant: v} }

// s1: n normal turns. v2v is end of speech to first bot audio.
func s1(c *client, cl clips, n int) *result {
	r := newResult("S1", "")
	for i := 0; i < n; i++ {
		c.quiet(400 * time.Millisecond)
		mark := time.Now()
		_, tEnd, err := c.stream(cl.q)
		if err != nil {
			r.add("error", err)
			return r
		}
		c.wait(15*time.Second, func() bool { return c.has(mark, "audio") })
		pfx := fmt.Sprintf("t%d.", i+1)
		if e, ok := c.firstAfter(mark, "audio"); ok {
			r.add(pfx+"v2v_ms", ms(e.t.Sub(tEnd)))
		} else {
			r.add(pfx+"v2v_ms", "TIMEOUT")
		}
		if e, ok := c.firstAfter(tEnd.Add(-300*time.Millisecond), "user-stopped-speaking"); ok {
			r.add(pfx+"vad_stop_ms", ms(e.t.Sub(tEnd)))
		}
		for _, e := range c.snap() {
			if e.t.After(mark) && e.kind == "user-transcription" && e.final {
				r.add(pfx+"final_tr_ms", ms(e.t.Sub(tEnd)))
				break
			}
		}
		c.wait(25*time.Second, func() bool { return c.has(tEnd, "bot-stopped-speaking") })
		r.add(pfx+"heard", fmt.Sprintf("%q", c.texts(mark, "user-transcription", true)))
		r.add(pfx+"reply", fmt.Sprintf("%q", truncate(c.texts(mark, "bot-llm-text", false), 90)))
		c.quiet(time.Second)
	}
	return r
}

// bargeSetup streams the Mars question and returns when the bot has been audible for 2 s.
func bargeSetup(c *client, cl clips, r *result) (ok bool) {
	c.quiet(400 * time.Millisecond)
	_, tEnd, _ := c.stream(cl.mars)
	if !c.wait(20*time.Second, func() bool { return c.has(tEnd, "audio") }) {
		r.add("error", "no bot audio for the setup question")
		return false
	}
	c.quiet(2 * time.Second)
	if !c.audioNow(300 * time.Millisecond) {
		r.add("error", "bot not audible 2 s after first audio")
		return false
	}
	return true
}

// s2: barge-in with a full utterance; checks flush time and that the second utterance is answered.
func s2(c *client, cl clips) *result {
	r := newResult("S2", "")
	if !bargeSetup(c, cl, r) {
		return r
	}
	onset, tEnd, _ := c.stream(cl.q)
	stop, paused := c.audioRunEnd(onset, 250*time.Millisecond)
	if !paused {
		r.add("flushed", false)
	} else {
		r.add("flushed", true)
		r.add("audio_stop_ms", ms(stop.Sub(onset)))
	}
	if e, ok := c.firstAfter(onset, "bot-stopped-speaking"); ok {
		r.add("bot_stopped_evt_ms", ms(e.t.Sub(onset)))
	}
	if e, ok := c.firstAfter(onset, "user-started-speaking"); ok {
		r.add("user_started_evt_ms", ms(e.t.Sub(onset)))
	}
	got := c.wait(20*time.Second, func() bool {
		return c.has(tEnd, "audio") && c.has(tEnd, "bot-llm-started")
	})
	if e, ok := c.firstAfter(tEnd, "bot-llm-started"); ok && got {
		r.add("llm_start_after_end_ms", ms(e.t.Sub(tEnd)))
	}
	if e, ok := c.firstAfter(tEnd, "audio"); ok {
		r.add("second_reply_v2v_ms", ms(e.t.Sub(tEnd)))
	} else {
		r.add("second_reply_v2v_ms", "NONE")
	}
	c.wait(25*time.Second, func() bool { return c.has(tEnd, "bot-stopped-speaking") })
	r.add("heard_after_onset", fmt.Sprintf("%q", c.texts(onset.Add(-500*time.Millisecond), "user-transcription", true)))
	r.add("reply_after_onset", fmt.Sprintf("%q", truncate(c.texts(onset, "bot-llm-text", false), 90)))
	return r
}

// s3: a backchannel while the bot speaks. With turn.min_words=3 it must not interrupt.
func s3(c *client, cl clips) *result {
	r := newResult("S3", "")
	if !bargeSetup(c, cl, r) {
		return r
	}
	onset, tEnd, _ := c.stream(cl.uhhuh)
	c.quiet(6 * time.Second) // long enough for a transcript, a model reply and its first audio
	r.add("new_reply_started", c.has(onset, "bot-llm-started"))
	r.add("bot_stopped_evts", c.count(onset, "bot-stopped-speaking"))
	stop, paused := c.audioRunEnd(onset, 250*time.Millisecond)
	if paused {
		r.add("audio_gap_after_ms", ms(stop.Sub(onset)))
	} else {
		r.add("audio_gap_after_ms", "none")
	}
	r.add("heard", fmt.Sprintf("%q", c.texts(onset.Add(-300*time.Millisecond), "user-transcription", true)))
	_ = tEnd
	c.wait(30*time.Second, func() bool {
		return c.has(tEnd.Add(3*time.Second), "bot-stopped-speaking") && !c.audioNow(1500*time.Millisecond)
	})
	return r
}

// s4: a ~1 s pause mid-utterance.
func s4(c *client, cl clips) *result {
	r := newResult("S4", "")
	c.quiet(400 * time.Millisecond)
	mark := time.Now()
	_, _, _ = c.stream(cl.pause[:len(cl.pause)/2*2])
	// the clip already contains the pause; end of speech = end of the stream
	tEnd := time.Now()
	c.wait(20*time.Second, func() bool { return c.has(mark, "audio") })
	if e, ok := c.firstAfter(mark, "audio"); ok {
		r.add("first_audio_vs_end_ms", ms(e.t.Sub(tEnd))) // negative: the bot spoke before the user finished
	} else {
		r.add("first_audio_vs_end_ms", "NONE")
	}
	c.wait(25*time.Second, func() bool { return c.has(tEnd, "bot-stopped-speaking") })
	c.quiet(4 * time.Second) // late replies
	r.add("user_turns", c.count(mark, "user-stopped-speaking"))
	r.add("bot_replies", c.count(mark, "bot-llm-started"))
	r.add("heard", fmt.Sprintf("%q", c.texts(mark, "user-transcription", true)))
	r.add("reply", fmt.Sprintf("%q", truncate(c.texts(mark, "bot-llm-text", false), 110)))
	return r
}

// s5: no speech at all: idle prompt then hang-up at short timings.
func s5(c *client) *result {
	r := newResult("S5", "")
	t0 := c.t0
	c.wait(25*time.Second, func() bool { return len(c.closed) > 0 })
	if e, ok := c.firstAfter(t0, "audio"); ok {
		r.add("first_prompt_audio_s", fmt.Sprintf("%.1f", e.t.Sub(t0).Seconds()))
	} else {
		r.add("first_prompt_audio_s", "NONE")
	}
	select {
	case err := <-c.closed:
		r.add("closed_after_s", fmt.Sprintf("%.1f", time.Since(t0).Seconds()))
		r.add("close_status", websocket.CloseStatus(err))
	default:
		r.add("closed_after_s", "NOT CLOSED")
	}
	r.add("bot_replies_text", fmt.Sprintf("%q", c.texts(t0, "bot-llm-text", false)))
	return r
}

// sttProbe streams a clip + trailing silence straight into Google streaming STT (one stream,
// no server) and prints when each interim/final result arrives. It shows what the
// turn manager's min_words gate and the VAD/STT timing have to work with.
func sttProbe(ctx context.Context, name string, pcm []byte, sa []byte) {
	cfg := google.DefaultSTTConfig()
	cfg.Model, cfg.LanguageCode, cfg.CredentialsJSON = sttModel, "en-US", sa
	op, err := google.NewRealGoogleSTTOpener(ctx, cfg)
	if err != nil {
		fmt.Println("stt probe:", redact(err))
		return
	}
	defer op.Close()
	st, err := op.OpenStream(ctx, cfg)
	if err != nil {
		fmt.Println("stt probe:", redact(err))
		return
	}
	t0 := time.Now()
	go func() {
		in := append(append([]byte(nil), pcm...), pcmSilence(4*time.Second)...)
		next := time.Now()
		for off := 0; off < len(in); off += chunkBytes {
			time.Sleep(time.Until(next))
			next = next.Add(chunkMS * time.Millisecond)
			if st.Send(in[off:min(off+chunkBytes, len(in))]) != nil {
				return
			}
		}
		_ = st.CloseSend()
	}()
	fmt.Printf("stt probe %s: speech ends at %.1fs\n", name, pcmDur(pcm).Seconds())
	for {
		rs, err := st.Recv()
		if err != nil {
			return
		}
		for _, r := range rs {
			fmt.Printf("  %5.2fs final=%v %q\n", time.Since(t0).Seconds(), r.IsFinal, r.Transcript)
		}
	}
}

// ---- main ----

func main() {
	root := flag.String("root", ".", "repo root")
	clipDir := flag.String("clips", "", "directory caching TTS-generated clips (default: temp dir, removed at exit)")
	only := flag.String("only", "", "comma list of scenarios S1..S5 (default all)")
	vars := flag.String("variants", "off,plain,on,fast,smart", "comma list of off,plain,on,fast,smart")
	turns := flag.Int("s1-turns", 3, "S1 turns per call")
	flag.StringVar(&sttModel, "stt-model", "chirp_2", "STT model for the agent and -stt-probe (chirp_2 gives no interim results; latest_long does)")
	prov := flag.String("providers", "google", "provider set for the agent: google or nvidia (needs NVIDIA_API_KEY, NVIDIA_STT_FUNCTION_ID, NVIDIA_TTS_FUNCTION_ID in examples/.env or the env)")
	flag.StringVar(&logLevel, "log-level", "info", "server log level")
	flag.DurationVar(&pauseLen, "pause", time.Second, "S4: silence inside the utterance")
	flag.BoolVar(&keep, "keep-work", false, "keep the temp dir (server logs, configs)")
	flag.BoolVar(&probe, "stt-probe", false, "only stream the question and pause clips to Google STT and print result timing (2 streams)")
	flag.BoolVar(&trace, "trace", false, "print the event timeline of every scenario")
	jsonOut := flag.String("json-out", "", "write results JSON here")
	flag.Parse()
	if os.Getenv("GOVOICE_LIVE") != "1" {
		fmt.Fprintln(os.Stderr, "livecheck spends real Google API credit: set GOVOICE_LIVE=1 to run")
		os.Exit(2)
	}
	nv = *prov == "nvidia"
	os.Exit(run(*root, *clipDir, *only, *vars, *turns, *jsonOut))
}

func run(root, clipDir, only, vars string, turns int, jsonOut string) int {
	root, _ = filepath.Abs(root)
	if err := os.Chdir(root); err != nil {
		fmt.Fprintln(os.Stderr, redact(err))
		return 1
	}
	_ = envload.Load()
	apiKey := os.Getenv("GOOGLE_API_KEY")
	var sa []byte
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			var j json.RawMessage
			if json.Unmarshal(b, &j) == nil {
				sa, _ = json.Marshal(j)
			}
		}
	}
	if nv {
		for _, k := range []string{"NVIDIA_API_KEY", "NVIDIA_STT_FUNCTION_ID", "NVIDIA_TTS_FUNCTION_ID"} {
			if os.Getenv(k) == "" {
				fmt.Fprintf(os.Stderr, "-providers nvidia: %s is not set\n", k)
				return 1
			}
		}
	} else if len(sa) == 0 || apiKey == "" {
		fmt.Fprintf(os.Stderr, "missing credentials in examples/.env: service account JSON readable=%v, GOOGLE_API_KEY set=%v (both needed: streaming STT rejects API keys)\n", len(sa) > 0, apiKey != "")
		return 1
	}
	work, err := os.MkdirTemp("", "govoice-livecheck-")
	if err != nil {
		fmt.Fprintln(os.Stderr, redact(err))
		return 1
	}
	if keep {
		fmt.Println("keeping", work, "(server logs and configs; mode 0700; each server's credential store is deleted when it stops)")
	} else {
		defer os.RemoveAll(work)
	}
	if clipDir == "" {
		clipDir = filepath.Join(work, "clips")
	}
	_ = os.MkdirAll(clipDir, 0o700)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	for _, b := range [][]string{{"-o", filepath.Join(work, "server"), "./cmd/server"}, {"-o", filepath.Join(work, "vadworker"), "./cmd/vadworker"}, {"-o", filepath.Join(work, "turnworker"), "./cmd/turnworker"}} {
		c := exec.Command("go", append([]string{"build"}, b...)...)
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "go build %v: %v\n%s", b, err, out)
			return 1
		}
	}
	q, err := loadWAV("e2e/fixtures/voice-debug.wav")
	if err != nil {
		fmt.Fprintln(os.Stderr, redact(err))
		return 1
	}
	genFn := genClips
	if nv {
		genFn = genClipsNvidia
	}
	g, err := genFn(ctx, clipDir, sa, apiKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, redact(err))
		return 1
	}
	if probe && !nv {
		sttProbe(ctx, "question", q, sa)
		sttProbe(ctx, "pause", append(append(append([]byte(nil), g["pauseA"]...), pcmSilence(pauseLen)...), g["pauseB"]...), sa)
		return 0
	}
	pause := append(append(append([]byte(nil), g["pauseA"]...), pcmSilence(pauseLen)...), g["pauseB"]...)
	cl := clips{q: q, mars: g["mars"], uhhuh: g["uhhuh"], pause: pause}
	fmt.Printf("clips: question %.1fs, mars %.1fs, backchannel %.1fs, pause clip %.1fs (incl %.1fs gap)\n",
		pcmDur(q).Seconds(), pcmDur(cl.mars).Seconds(), pcmDur(cl.uhhuh).Seconds(), pcmDur(pause).Seconds(), pauseLen.Seconds())

	want := func(s string) bool { return only == "" || strings.Contains(only, s) }
	smartStop := "turn:\n  manager: true\n  min_words: 3\n  stop: [turn-analyzer, speech-timeout]\n"
	all := map[string]variant{
		"off":    {name: "off"},
		"plain":  {name: "plain", turn: "turn:\n  manager: true\n"},
		"on":     {name: "on", turn: "turn:\n  manager: true\n  min_words: 3\n"},
		"smart":  {name: "smart", turn: smartStop},
		"smart0": {name: "smart0", turn: "turn:\n  manager: true\n  stop: [turn-analyzer, speech-timeout]\n"},
		"fast":   {name: "fast", turn: "turn:\n  manager: true\n  min_words: 3\n  speech_timeout: 150ms\n"},
	}
	idle := "idle: {user_timeout: 4s, max_prompts: 1, prompt: \"Are you still there?\", prompt_mode: say, goodbye: \"Goodbye.\"}\n"
	var results []*result
	for _, name := range strings.Split(vars, ",") {
		v, ok := all[name]
		if !ok {
			continue
		}
		srv, err := startServer(ctx, root, filepath.Join(work, "server"), work, v)
		if err != nil {
			fmt.Printf("variant %s: %v\n", name, redact(err))
			continue
		}
		agent, err := srv.provision(sa, apiKey)
		if err != nil {
			fmt.Printf("variant %s: provision: %v\n", name, redact(err))
			srv.stop()
			continue
		}
		type sc struct {
			id string
			fn func(*client) *result
		}
		for _, x := range []sc{
			{"S1", func(c *client) *result { return s1(c, cl, turns) }},
			{"S2", func(c *client) *result { return s2(c, cl) }},
			{"S3", func(c *client) *result { return s3(c, cl) }},
			{"S4", func(c *client) *result { return s4(c, cl) }},
		} {
			if !want(x.id) {
				continue
			}
			c, err := dial(ctx, srv, agent)
			if err != nil {
				fmt.Printf("%s %s: %v\n", x.id, name, redact(err))
				continue
			}
			r := x.fn(c)
			if trace {
				fmt.Println("  timeline", x.id, name)
				c.timeline()
				srv.timeline(c.n)
			}
			r.scenario, r.variant = x.id, name
			c.close()
			results = append(results, r)
			fmt.Printf("%s %-6s %s\n", r.scenario, r.variant, strings.Join(r.metrics, " "))
		}
		fmt.Printf("  [%s server stage means] %s\n", name, srv.stageMeans())
		for _, l := range srv.logProblems() {
			fmt.Printf("  [%s log] %s\n", name, l)
		}
		srv.stop()
	}
	if want("S5") {
		for _, name := range []string{"off", "on"} {
			v := all[name]
			v.name, v.idle = "idle-"+name, idle
			srv, err := startServer(ctx, root, filepath.Join(work, "server"), work, v)
			if err != nil {
				fmt.Printf("S5 %s: %v\n", name, redact(err))
				continue
			}
			agent, err := srv.provision(sa, apiKey)
			if err == nil {
				var c *client
				if c, err = dial(ctx, srv, agent); err == nil {
					r := s5(c)
					if trace {
						c.timeline()
					}
					r.scenario, r.variant = "S5", name
					_ = c.conn.CloseNow()
					results = append(results, r)
					fmt.Printf("S5 %-6s %s\n", name, strings.Join(r.metrics, " "))
				}
			}
			if err != nil {
				fmt.Printf("S5 %s: %v\n", name, redact(err))
			}
			for _, l := range srv.logProblems() {
				fmt.Printf("  [idle-%s log] %s\n", name, l)
			}
			srv.stop()
		}
	}
	if jsonOut != "" {
		b, _ := json.MarshalIndent(results, "", " ")
		_ = os.WriteFile(jsonOut, b, 0o600)
	}
	return 0
}
