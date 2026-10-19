// Package stack spawns and drives a server child for the free harnesses
// (loadtest, livecheck): ports, config, process group, readiness, management
// API, voice tokens and the RTVI handshake.
package stack

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
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"govoice/transport/ws"
)

// Spec describes the child to start.
type Spec struct {
	Root, Bin, Dir string                    // repo root, server binary, per-run work dir (config + data + log)
	Config         func(pub, adm int) []byte // server YAML on the chosen ports
	Env            []string                  // child environment besides the generated secrets (caller decides: allowlist or environ)
	Args           []string                  // extra server flags
}

// Server is a running child. Public is host:port, Admin a base URL. Key is the
// management API key; callers must never print or log it.
type Server struct {
	Public, Admin, Key, Log, Data string
	cmd                           *exec.Cmd
	done                          chan struct{}
}

// RandHex returns 2n hex characters of crypto randomness.
func RandHex(n int) string {
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

// readyClient bounds each /ready probe, so a server that accepts and never answers cannot outlast Start's deadline.
var readyClient = &http.Client{Timeout: 2 * time.Second}

// Start spawns the child in its own process group and waits up to 60 s for /ready.
func Start(ctx context.Context, sp Spec) (*Server, error) {
	ports, err := freePorts(2)
	if err != nil {
		return nil, err
	}
	data := filepath.Join(sp.Dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(sp.Dir, "server.yaml")
	if err := os.WriteFile(cfgPath, sp.Config(ports[0], ports[1]), 0o600); err != nil {
		return nil, err
	}
	s := &Server{Public: fmt.Sprintf("127.0.0.1:%d", ports[0]), Admin: fmt.Sprintf("http://127.0.0.1:%d", ports[1]),
		Key: RandHex(32), Log: filepath.Join(sp.Dir, "server.log"), Data: data, done: make(chan struct{})}
	logf, err := os.Create(s.Log)
	if err != nil {
		return nil, err
	}
	// ponytail: exec.Command, not CommandContext: cleanup must signal the group, not just the pid.
	s.cmd = exec.Command(sp.Bin, append([]string{"-config", cfgPath}, sp.Args...)...)
	s.cmd.Dir = sp.Root
	s.cmd.Stdout, s.cmd.Stderr = logf, logf
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// ENV_FILE=/dev/null: a development server would otherwise load examples/.env itself.
	s.cmd.Env = append(append([]string(nil), sp.Env...), "GOVOICE_ROOT="+sp.Root, "GOVOICE_DEV_DIR="+data,
		"GOVOICE_MANAGEMENT_API_KEY="+s.Key, "TOKEN_SECRET="+RandHex(32), "ENV_FILE=/dev/null")
	if err := s.cmd.Start(); err != nil {
		logf.Close()
		return nil, err
	}
	go func() { _ = s.cmd.Wait(); logf.Close(); close(s.done) }()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		select {
		case <-s.done:
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL) // sweep what the exited server left in its group
			return nil, fmt.Errorf("server exited during start (log: %s)", s.Log)
		case <-ctx.Done():
			s.Stop()
			return nil, ctx.Err()
		default:
		}
		if r, err := readyClient.Get(s.Admin + "/ready"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return s, nil
			}
		}
	}
	s.Stop()
	return nil, fmt.Errorf("server not ready in 60s (log: %s)", s.Log)
}

// PID is the child's pid, 0 when there is no child.
func (s *Server) PID() int {
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

// Stop sends SIGTERM to the group, waits up to 15 s, then SIGKILLs the group
// (sweeps vadworker stragglers; ESRCH when none). Idempotent; no-op without a child.
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
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	<-s.done
}

// Call sends one management API request with the generated key and returns the
// response's data field. Response bodies are never echoed: they could contain secrets.
func (s *Server) Call(method, path string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.Admin+"/api/v1/"+path, rd)
	if err != nil {
		return nil, fmt.Errorf("%s %s: bad request", method, path)
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
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

// Token mints a single-use voice token.
func (s *Server) Token(principal, flowID string) (string, error) {
	d, err := s.Call("POST", "voice-tokens", map[string]string{"principal": principal, "flow_id": flowID})
	if err != nil {
		return "", err
	}
	var t struct{ Token string }
	if err := json.Unmarshal(d, &t); err != nil || t.Token == "" {
		return "", fmt.Errorf("token response: %v", err)
	}
	return t.Token, nil
}

// Handshake sends RTVI client-ready and waits up to 20 s for bot-ready.
func Handshake(ctx context.Context, conn *websocket.Conn, pushToTalk bool) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ptt := ""
	if pushToTalk {
		ptt = `,"push_to_talk":true`
	}
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"` + ptt + `}}`)
	if err := conn.Write(ctx, websocket.MessageBinary, ready); err != nil {
		return fmt.Errorf("client-ready: %w", err)
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("no bot-ready: %w", err)
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			if m, ok := f.(*ws.WireMessageFrame); ok && strings.Contains(m.Data, "bot-ready") {
				return nil
			}
		}
	}
}
