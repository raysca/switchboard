package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"govoice/server/api"
	"govoice/server/demo"
	"govoice/server/mcp"
	"govoice/server/storage"
)

// demoServers adapts the MCP service and store to demo.Servers.
type demoServers struct {
	svc   *mcp.Service
	store *storage.Store
}

func (d demoServers) List(ctx context.Context) ([]storage.MCPServer, error) {
	return d.store.MCP().ListServers(ctx)
}
func (d demoServers) Create(ctx context.Context, c mcp.ServerConfig) (storage.MCPServer, error) {
	return d.svc.Create(ctx, c)
}
func (d demoServers) Enable(ctx context.Context, id string) error { return d.svc.Enable(ctx, id) }
func (d demoServers) Refresh(ctx context.Context, id string) (storage.MCPDiscovery, error) {
	return d.svc.Refresh(ctx, id)
}

// dentalEndpoint is where the demo's MCP server lives; empty when the deployment has none.
func (s *Server) dentalEndpoint() string { return s.env("DENTAL_MCP_URL") }

// demoInstallTimeout bounds one shared install (discovery plus DB writes).
const demoInstallTimeout = 30 * time.Second

// errDemoShuttingDown refuses an install once Shutdown began: it must not run against a closing store.
var errDemoShuttingDown = errors.New("demo install refused: the server is shutting down")

// installDental collapses overlapping installs (startup auto-install vs the API)
// into one run, so the list-then-create inside cannot run twice; no lock is held
// during discovery or DB work. The run is shared by every caller, so it does
// not use any caller's context: it runs on its own, bounded, which Shutdown
// cancels (stopDemoLocked) and then waits for (waitDemo, through demoWG) before
// it closes the store (the flight has its own 30 s bound, so a startup install can outlive
// autoInstallDemo's 60 s context; still bounded, and Shutdown cancels it). Each caller waits for the result or for its own ctx.
func (s *Server) installDental(ctx context.Context, servers demo.Servers, flows demo.Flows, endpoint string) (demo.Result, error) {
	ch := s.demoSF.DoChan("dental", func() (res any, err error) {
		// DoChan runs this on its own goroutine: neither net/http's per-request recover nor
		// autoInstallDemo's covers it, and a panic here would end the process.
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("demo install panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				res, err = demo.Result{}, fmt.Errorf("demo install panicked: %v", r)
			}
		}()
		fctx, cancel := context.WithTimeout(context.Background(), demoInstallTimeout)
		defer cancel()
		s.mu.Lock()
		if s.shuttingDown.Load() {
			s.mu.Unlock()
			return demo.Result{}, errDemoShuttingDown
		}
		s.demoWG.Add(1) // under mu with the check, so Shutdown's waitDemo cannot see zero first
		s.stopFlight = cancel
		s.mu.Unlock()
		defer s.demoWG.Done()
		defer func() {
			s.mu.Lock()
			s.stopFlight = nil
			s.mu.Unlock()
		}()
		return demo.InstallDental(fctx, servers, flows, endpoint)
	})
	select {
	case r := <-ch:
		res, _ := r.Val.(demo.Result)
		return res, r.Err
	case <-ctx.Done():
		return demo.Result{}, ctx.Err()
	}
}

// stopDemoLocked cancels the startup auto-install and the running install
// flight (Shutdown, with s.mu held).
func (s *Server) stopDemoLocked() {
	if s.stopDemo != nil {
		s.stopDemo()
	}
	if s.stopFlight != nil {
		s.stopFlight()
	}
}

// demoStatus reports the demo's state; a failed lookup is an error, not "not installed".
func demoStatus(ctx context.Context, flows demo.Flows, endpoint string) (demoStatusResp, error) {
	var st demoStatusResp
	st.Dental.Available = endpoint != ""
	f, err := demo.FindFlow(ctx, flows)
	if err != nil {
		return st, err
	}
	if f.ID != "" {
		st.Dental.Installed, st.Dental.FlowID = true, f.ID
	}
	return st, nil
}

type demoStatusResp struct {
	Dental struct {
		Available bool   `json:"available"`
		Installed bool   `json:"installed"`
		FlowID    string `json:"flow_id,omitempty"`
	} `json:"dental"`
}

func (s *Server) handleDemo(w http.ResponseWriter, r *http.Request, requestID string) {
	if s.mgmt == nil || s.flows == nil || s.mcpService == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "management_unavailable", "management storage is unavailable", requestID)
		return
	}
	switch {
	case r.URL.Path == "/api/v1/demo" && r.Method == http.MethodGet:
		st, err := demoStatus(r.Context(), s.flows, s.dentalEndpoint())
		if err != nil {
			s.logger.Warn("demo status failed", "err", s.redactor.Redact(err.Error()))
			api.WriteError(w, http.StatusInternalServerError, "internal_error", "the demo status could not be read", requestID)
			return
		}
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[demoStatusResp]{Data: st})
	case r.URL.Path == "/api/v1/demo/dental" && r.Method == http.MethodPost:
		if s.dentalEndpoint() == "" {
			api.WriteError(w, http.StatusConflict, "demo_unavailable", "DENTAL_MCP_URL is not set on this server", requestID)
			return
		}
		res, err := s.installDental(r.Context(), demoServers{svc: s.mcpService, store: s.mgmt.store}, s.flows, s.dentalEndpoint())
		if err != nil {
			s.logger.Warn("demo install failed", "err", s.redactor.Redact(err.Error()))
			api.WriteError(w, http.StatusBadGateway, "demo_install_failed", "the demo could not be installed; check the server log", requestID)
			return
		}
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[demo.Result]{Data: res})
	default:
		w.Header().Set("Allow", "GET, POST")
		api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
	}
}

// waitDemo waits for the startup install to end; false means ctx expired first.
func (s *Server) waitDemo(ctx context.Context) bool {
	done := make(chan struct{})
	go func() { s.demoWG.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// autoInstallDemo installs the dental demo in the background. A failure is logged loudly and
// never stops the server: the admin UI button can retry.
func (s *Server) autoInstallDemo() {
	// Capture the services now: management teardown can nil s.mgmt while the goroutine runs.
	m, flows, svc := s.mgmt, s.flows, s.mcpService
	if m == nil || flows == nil || svc == nil {
		s.logger.Error("demo.auto_install is dental but management storage is disabled")
		return
	}
	endpoint := s.dentalEndpoint()
	if endpoint == "" {
		s.logger.Error("demo.auto_install is dental but DENTAL_MCP_URL is empty")
		return
	}
	// Bounded by 60s and cancelled by Shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	s.mu.Lock()
	if s.shuttingDown.Load() { // Shutdown may already have waited: do not start behind its back
		s.mu.Unlock()
		cancel()
		return
	}
	s.stopDemo = cancel
	s.demoWG.Add(1) // under mu with the check, so Shutdown's Wait cannot see zero first
	s.mu.Unlock()
	go func() {
		defer s.demoWG.Done()
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("demo auto-install panicked", "panic", fmt.Sprint(r))
			}
		}()
		defer cancel()
		// The MCP service may still be cold-starting (Cloud Run scales it to zero): retry until the 60s end.
		var res demo.Result
		err := retryUntil(ctx, demoRetryEvery, func(ctx context.Context) (err error) {
			res, err = s.installDental(ctx, demoServers{svc: svc, store: m.store}, flows, endpoint)
			return err
		}, func(err error) { s.logger.Warn("demo auto-install will retry", "err", s.redactor.Redact(err.Error())) })
		if err != nil {
			s.logger.Error("demo auto-install failed", "err", s.redactor.Redact(err.Error()))
			return
		}
		s.logger.Info("demo installed", "flow", res.FlowID, "flow_created", res.FlowCreated, "mcp_created", res.MCPCreated)
	}()
}

// demoRetryEvery is the pause between auto-install attempts.
const demoRetryEvery = 3 * time.Second

// retryUntil calls fn until it succeeds or ctx ends, pausing `every` between attempts; onRetry (optional)
// sees each failure that will be retried. It returns nil, or the last error when ctx ended first.
func retryUntil(ctx context.Context, every time.Duration, fn func(context.Context) error, onRetry func(error)) error {
	for {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if onRetry != nil {
			onRetry(err)
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}
