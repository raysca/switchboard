package server

import (
	"context"
	"fmt"
	"net/http"
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

// installDental serialises installs (startup auto-install vs the API) so the
// list-then-create inside cannot run twice; the install is bounded by ctx.
func (s *Server) installDental(ctx context.Context, servers demo.Servers, flows demo.Flows, endpoint string) (demo.Result, error) {
	s.demoMu.Lock()
	defer s.demoMu.Unlock()
	return demo.InstallDental(ctx, servers, flows, endpoint)
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
	s.stopDemo = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Error("demo auto-install panicked", "panic", fmt.Sprint(r))
			}
		}()
		defer cancel()
		res, err := s.installDental(ctx, demoServers{svc: svc, store: m.store}, flows, endpoint)
		if err != nil {
			s.logger.Error("demo auto-install failed", "err", s.redactor.Redact(err.Error()))
			return
		}
		s.logger.Info("demo installed", "flow", res.FlowID, "flow_created", res.FlowCreated, "mcp_created", res.MCPCreated)
	}()
}
