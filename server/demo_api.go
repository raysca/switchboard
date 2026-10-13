package server

import (
	"context"
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
func (d demoServers) Refresh(ctx context.Context, id string) (storage.MCPDiscovery, error) {
	return d.svc.Refresh(ctx, id)
}

// dentalEndpoint is where the demo's MCP server lives; empty when the deployment has none.
func (s *Server) dentalEndpoint() string { return s.env("DENTAL_MCP_URL") }

// InstallDentalDemo runs the demo installer with this server's services.
func (s *Server) InstallDentalDemo(ctx context.Context) (demo.Result, error) {
	return demo.InstallDental(ctx, demoServers{svc: s.mcpService, store: s.mgmt.store}, s.flows, s.dentalEndpoint())
}

type demoStatus struct {
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
		var st demoStatus
		st.Dental.Available = s.dentalEndpoint() != ""
		if id, err := demo.FindFlow(r.Context(), s.flows); err == nil && id != "" {
			st.Dental.Installed, st.Dental.FlowID = true, id
		}
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[demoStatus]{Data: st})
	case r.URL.Path == "/api/v1/demo/dental" && r.Method == http.MethodPost:
		if s.dentalEndpoint() == "" {
			api.WriteError(w, http.StatusConflict, "demo_unavailable", "DENTAL_MCP_URL is not set on this server", requestID)
			return
		}
		res, err := s.InstallDentalDemo(r.Context())
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
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if s.dentalEndpoint() == "" {
			s.logger.Error("demo.auto_install is dental but DENTAL_MCP_URL is empty")
			return
		}
		res, err := s.InstallDentalDemo(ctx)
		if err != nil {
			s.logger.Error("demo auto-install failed", "err", s.redactor.Redact(err.Error()))
			return
		}
		s.logger.Info("demo installed", "flow", res.FlowID, "flow_created", res.FlowCreated, "mcp_created", res.MCPCreated)
	}()
}
