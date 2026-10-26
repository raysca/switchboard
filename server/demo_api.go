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
	"govoice/server/httptool"
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

// demoHTTPTools adapts the HTTP tool service to demo.HTTPTools.
type demoHTTPTools struct{ svc *httptool.Service }

func (d demoHTTPTools) List(ctx context.Context) ([]httptool.View, error) { return d.svc.List(ctx) }
func (d demoHTTPTools) Create(ctx context.Context, def httptool.Definition) (httptool.View, error) {
	return d.svc.Create(ctx, def)
}
func (d demoHTTPTools) SetEnabled(ctx context.Context, id string, enabled bool) (httptool.View, error) {
	return d.svc.SetEnabled(ctx, id, enabled)
}

// dentalEndpoint is where the dental demo's MCP server lives; empty when the deployment has none.
func (s *Server) dentalEndpoint() string { return s.env("DENTAL_MCP_URL") }

// atlasEndpoint is the Atlas demo's base URL (MCP at /mcp, HTTP tools at /api/...); empty when absent.
func (s *Server) atlasEndpoint() string { return s.env("ATLAS_DEMO_URL") }

// atlasToken is the env var name of the Atlas MCP bearer token; the demo needs both.
func (s *Server) atlasToken() string { return s.env("MCP_TOKEN_ATLAS") }

// demoInstallTimeout bounds one shared install (discovery plus DB writes).
const demoInstallTimeout = 30 * time.Second

// errDemoShuttingDown refuses an install once Shutdown began: it must not run against a closing store.
var errDemoShuttingDown = errors.New("demo install refused: the server is shutting down")

// installDemo collapses overlapping installs of one demo (startup auto-install
// vs the API) into one run, so the list-then-create inside cannot run twice; no
// lock is held during discovery or DB work. The run is shared by every caller,
// so it does not use any caller's context: it runs on its own, bounded, which
// Shutdown cancels (stopDemoLocked) and then waits for (waitDemo, through
// demoWG) before it closes the store. Each caller waits for the result or for
// its own ctx. installs of different demos are independent and both count on
// demoWG, so Shutdown cancels and waits for every one of them.
func (s *Server) installDemo(ctx context.Context, name string, run func(context.Context) (demo.Result, error)) (demo.Result, error) {
	ch := s.demoSF.DoChan(name, func() (res any, err error) {
		// DoChan runs this on its own goroutine: neither net/http's per-request recover nor
		// autoInstallDemo's covers it, and a panic here would end the process.
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("demo install panicked", "demo", name, "panic", s.redactor.Redact(fmt.Sprint(r)), "stack", s.redactor.Redact(string(debug.Stack())))
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
		if s.stopFlights == nil {
			s.stopFlights = make(map[string]context.CancelFunc, 2)
		}
		s.stopFlights[name] = cancel
		s.mu.Unlock()
		defer s.demoWG.Done()
		defer func() {
			s.mu.Lock()
			delete(s.stopFlights, name)
			s.mu.Unlock()
		}()
		return run(fctx)
	})
	select {
	case r := <-ch:
		res, _ := r.Val.(demo.Result)
		return res, r.Err
	case <-ctx.Done():
		return demo.Result{}, ctx.Err()
	}
}

// installDental installs the Smile Dental demo.
func (s *Server) installDental(ctx context.Context, servers demo.Servers, agents demo.Agents, endpoint string) (demo.Result, error) {
	return s.installDemo(ctx, "dental", func(fctx context.Context) (demo.Result, error) {
		return demo.InstallDental(fctx, servers, agents, endpoint)
	})
}

// installAtlas installs the Atlas Travel demo.
func (s *Server) installAtlas(ctx context.Context, servers demo.Servers, tools demo.HTTPTools, agents demo.Agents, base string) (demo.Result, error) {
	return s.installDemo(ctx, "atlas", func(fctx context.Context) (demo.Result, error) {
		return demo.InstallAtlas(fctx, servers, tools, agents, base)
	})
}

// stopDemoLocked cancels the startup auto-install and every running install
// flight (Shutdown, with s.mu held).
func (s *Server) stopDemoLocked() {
	if s.stopDemo != nil {
		s.stopDemo()
	}
	for _, cancel := range s.stopFlights {
		cancel()
	}
}

// demoStatus reports each demo's state; a failed lookup is an error, not "not installed".
func demoStatus(ctx context.Context, agents demo.Agents, dentalEndpoint, atlasEndpoint, atlasToken string) (demoStatusResp, error) {
	var st demoStatusResp
	st.Dental.Available = dentalEndpoint != ""
	// The Atlas card is offered only when the server has both halves of the demo:
	// an install without the token fails at discovery with a generic error.
	st.Atlas.Available = atlasEndpoint != "" && atlasToken != ""
	found, err := demo.FindAgentsNamed(ctx, agents, demo.AgentName, demo.AtlasAgentName)
	if err != nil {
		return st, err
	}
	if f := found[demo.AgentName]; f.ID != "" {
		st.Dental.Installed, st.Dental.AgentID = true, f.ID
	}
	if f := found[demo.AtlasAgentName]; f.ID != "" {
		st.Atlas.Installed, st.Atlas.AgentID = true, f.ID
	}
	return st, nil
}

// demoStatusEntry is one demo's availability in GET /api/v1/demo.
type demoStatusEntry struct {
	Available bool   `json:"available"`
	Installed bool   `json:"installed"`
	AgentID   string `json:"agent_id,omitempty"`
}

type demoStatusResp struct {
	Dental demoStatusEntry `json:"dental"`
	Atlas  demoStatusEntry `json:"atlas"`
}

func (s *Server) handleDemo(w http.ResponseWriter, r *http.Request, requestID string) {
	mgmt := s.mgmt.Load()
	if mgmt == nil || s.agents == nil || s.mcpService == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "management_unavailable", "management storage is unavailable", requestID)
		return
	}
	servers := demoServers{svc: s.mcpService, store: mgmt.store}
	switch {
	case r.URL.Path == "/api/v1/demo" && r.Method == http.MethodGet:
		st, err := demoStatus(r.Context(), s.agents, s.dentalEndpoint(), s.atlasEndpoint(), s.atlasToken())
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
		res, err := s.installDental(r.Context(), servers, s.agents, s.dentalEndpoint())
		if err != nil {
			s.logger.Warn("demo install failed", "demo", "dental", "err", s.redactor.Redact(err.Error()))
			api.WriteError(w, http.StatusBadGateway, "demo_install_failed", "the demo could not be installed; check the server log", requestID)
			return
		}
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[demo.Result]{Data: res})
	case r.URL.Path == "/api/v1/demo/atlas" && r.Method == http.MethodPost:
		if s.atlasEndpoint() == "" {
			api.WriteError(w, http.StatusConflict, "demo_unavailable", "ATLAS_DEMO_URL is not set on this server", requestID)
			return
		}
		if s.atlasToken() == "" {
			api.WriteError(w, http.StatusConflict, "demo_unavailable", "MCP_TOKEN_ATLAS is not set on this server", requestID)
			return
		}
		if s.httpTools == nil { // management storage off; the guard above already refuses this
			api.WriteError(w, http.StatusServiceUnavailable, "management_unavailable", "management storage is unavailable", requestID)
			return
		}
		res, err := s.installAtlas(r.Context(), servers, demoHTTPTools{svc: s.httpTools}, s.agents, s.atlasEndpoint())
		if err != nil {
			s.logger.Warn("demo install failed", "demo", "atlas", "err", s.redactor.Redact(err.Error()))
			api.WriteError(w, http.StatusBadGateway, "demo_install_failed", "the demo could not be installed; check the server log", requestID)
			return
		}
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[demo.Result]{Data: res})
	default:
		w.Header().Set("Allow", "GET, POST")
		api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
	}
}

// waitDemo waits for running installs to end; false means ctx expired first.
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

// autoInstallDemo installs the demo named by demo.auto_install in the
// background. A failure is logged loudly and never stops the server: the admin
// UI button can retry.
func (s *Server) autoInstallDemo() {
	name := s.cfg.Demo.AutoInstall
	// Capture the services now: management teardown can nil s.mgmt while the goroutine runs.
	m, agents, svc, tools := s.mgmt.Load(), s.agents, s.mcpService, s.httpTools
	if m == nil || agents == nil || svc == nil || (name == "atlas" && tools == nil) {
		s.logger.Error("demo.auto_install is set but management storage is disabled", "demo", name)
		return
	}
	var endpoint string
	switch name {
	case "dental":
		endpoint = s.dentalEndpoint()
		if endpoint == "" {
			s.logger.Error("demo.auto_install is dental but DENTAL_MCP_URL is empty")
			return
		}
	case "atlas":
		endpoint = s.atlasEndpoint()
		if endpoint == "" {
			s.logger.Error("demo.auto_install is atlas but ATLAS_DEMO_URL is empty")
			return
		}
		if s.atlasToken() == "" {
			s.logger.Error("demo.auto_install is atlas but MCP_TOKEN_ATLAS is empty")
			return
		}
	default:
		return // ValidateConfig refuses anything else
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
	servers := demoServers{svc: svc, store: m.store}
	go func() {
		defer s.demoWG.Done()
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("demo auto-install panicked", "demo", name, "panic", s.redactor.Redact(fmt.Sprint(r)))
			}
		}()
		defer cancel()
		// The demo services may still be cold-starting (Cloud Run scales to zero): retry until the 60s end.
		var res demo.Result
		err := retryUntil(ctx, demoRetryEvery, func(ctx context.Context) (err error) {
			if name == "atlas" {
				res, err = s.installAtlas(ctx, servers, demoHTTPTools{svc: tools}, agents, endpoint)
				return err
			}
			res, err = s.installDental(ctx, servers, agents, endpoint)
			return err
		}, func(err error) {
			s.logger.Warn("demo auto-install will retry", "demo", name, "err", s.redactor.Redact(err.Error()))
		})
		if err != nil {
			s.logger.Error("demo auto-install failed", "demo", name, "err", s.redactor.Redact(err.Error()))
			return
		}
		s.logger.Info("demo installed", "demo", name, "agent", res.AgentID, "agent_created", res.AgentCreated, "mcp_created", res.MCPCreated)
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
