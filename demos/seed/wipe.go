package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wipe removes the demo-owned resources in dependency order: live agents, then
// MCP servers (polling until each row is gone), then the token secrets. HTTP
// tools are never deleted: their historical pins make deletion impossible, and
// create reuses them by exact name.
func (r *runner) wipe(ctx context.Context, targets []target, liveAgents []agentRow, servers []mcpServerRow) error {
	for _, a := range liveAgents {
		if _, err := r.c.do(ctx, http.MethodPost, "/api/v1/agents/"+url.PathEscape(a.ID)+"/archive", nil, nil); err != nil {
			return fmt.Errorf("archive demo agent %q: %w", a.Name, err)
		}
		r.act("archived demo agent %q", a.Name)
		r.report.Wiped.AgentsArchived++
	}
	if len(servers) > 0 {
		pollCtx, cancel := context.WithTimeout(ctx, r.opts.wait)
		defer cancel()
		for _, m := range servers {
			if err := r.detachDelete(pollCtx, m); err != nil {
				return err
			}
		}
	}
	for _, t := range targets {
		status, err := r.c.do(ctx, http.MethodDelete, "/api/v1/secrets/"+url.PathEscape(t.spec.SecretName), nil, nil, http.StatusNoContent, http.StatusNotFound)
		if err != nil {
			return fmt.Errorf("delete token secret %q: %w", t.spec.SecretName, err)
		}
		if status == http.StatusNoContent {
			r.act("deleted token secret %q", t.spec.SecretName)
			r.report.Wiped.SecretsDeleted++
		} else {
			r.act("token secret %q was already absent", t.spec.SecretName)
		}
	}
	return nil
}

// detachDelete detaches and deletes one MCP server, polling while the server
// reports the deletion pending. The poll context bounds the wait.
func (r *runner) detachDelete(ctx context.Context, m mcpServerRow) error {
	path := "/api/v1/mcp-servers/" + url.PathEscape(m.ID)
	var detached envelope[detachResult]
	status, err := r.c.do(ctx, http.MethodPost, path+"/detach-delete", map[string]bool{"confirm": true}, &detached, http.StatusOK, http.StatusAccepted, http.StatusNotFound)
	if err != nil {
		return fmt.Errorf("detach-delete MCP server %q: %w", m.Name, err)
	}
	if status == http.StatusNotFound || status == http.StatusOK && !detached.Data.Pending {
		r.act("deleted MCP server %q", m.Name)
		r.report.Wiped.MCPServersDeleted++
		return nil
	}
	if err := r.waitServerGone(ctx, m); err != nil {
		return err
	}
	r.act("deleted MCP server %q", m.Name)
	r.report.Wiped.MCPServersDeleted++
	return nil
}

// waitServerGone polls the MCP server row until it is gone. A 202 from
// detach-delete means a call still pins it, so the row disappears later.
func (r *runner) waitServerGone(ctx context.Context, m mcpServerRow) error {
	ticker := time.NewTicker(r.opts.pollInterval)
	defer ticker.Stop()
	for {
		var detail envelope[mcpServerDetail]
		status, err := r.c.do(ctx, http.MethodGet, "/api/v1/mcp-servers/"+url.PathEscape(m.ID), nil, &detail, http.StatusOK, http.StatusNotFound)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("MCP server %q (id %s) is still pending deletion after %s: %w", m.Name, m.ID, r.opts.wait, ctx.Err())
			}
			return fmt.Errorf("wait for MCP server %q deletion: %w", m.Name, err)
		}
		if status == http.StatusNotFound {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("MCP server %q (id %s) is still pending deletion after %s: %w", m.Name, m.ID, r.opts.wait, ctx.Err())
		case <-ticker.C:
		}
	}
}

// plan lists what a real run would do without writing anything. It reads the
// server to count the existing demo resources and to tell create from update.
func (r *runner) plan(ctx context.Context, targets []target, httpBase, agentName string, liveAgents []agentRow, servers []mcpServerRow) (report, error) {
	for _, a := range liveAgents {
		r.act("would archive demo agent %q (id %s)", a.Name, a.ID)
		r.report.Wiped.AgentsArchived++
	}
	for _, m := range servers {
		r.act("would detach-delete MCP server %q (id %s) and poll until the row is gone", m.Name, m.ID)
		r.report.Wiped.MCPServersDeleted++
	}
	for _, t := range targets {
		r.act("would delete token secret %q (404 counts as success)", t.spec.SecretName)
		r.report.Wiped.SecretsDeleted++
	}
	if r.opts.wipeOnly {
		count := len(r.report.Actions)
		r.act("dry run: %d write action(s); only read-only requests were sent", count)
		return r.report, nil
	}
	for _, t := range targets {
		r.act("would write token secret %q from environment variable %s", t.spec.SecretName, t.spec.TokenEnv)
		r.act("would create MCP server %q at %s (transport %s, auth_env %q, enabled)", t.spec.Name, t.endpoint, transportStreamableHTTP, t.spec.SecretName)
		r.act("would validate MCP server %q and require its tools: %s", t.spec.Name, strings.Join(t.spec.Tools, ", "))
	}
	httpTools, err := r.listHTTPTools(ctx)
	if err != nil {
		return r.report, err
	}
	byName := make(map[string]httpToolView, len(httpTools))
	for _, t := range httpTools {
		byName[t.Name] = t
	}
	pins := 0
	for _, t := range r.opts.demo.MCPServers() {
		pins += len(t.Tools)
	}
	for _, h := range r.opts.demo.HTTPTools() {
		toolURL, err := joinURL(httpBase, h.Path)
		if err != nil {
			return r.report, fmt.Errorf("HTTP tool %q: %w", h.Name, err)
		}
		expected := httpDefinitionRequest{Name: h.Name, Description: h.Description, Method: strings.ToUpper(strings.TrimSpace(h.Method)), URL: toolURL, InputSchema: h.InputSchema}
		current, exists := byName[h.Name]
		switch {
		case !exists:
			r.act("would create HTTP tool %q at %s", h.Name, toolURL)
		case httpDefinitionDiffers(current.Definition, expected):
			if r.opts.yes {
				r.act("would update HTTP tool %q to the expected definition", h.Name)
			} else {
				r.act("would refuse: HTTP tool %q has another definition and --yes was not passed", h.Name)
			}
		default:
			r.act("would keep HTTP tool %q and pin its current hash", h.Name)
		}
		pins++
	}
	r.act("would create agent %q with %d tool pin(s)", agentName, pins)
	count := len(r.report.Actions)
	r.act("dry run: %d action(s); only read-only requests were sent", count)
	return r.report, nil
}
