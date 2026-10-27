package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"

	"govoice/demos/spec"
)

// create writes the token secrets, MCP servers, HTTP tools and agent, then
// verifies the agent by the id the server returned.
func (r *runner) create(ctx context.Context, targets []target, httpBase string) error {
	resolution := spec.Resolution{ServerIDs: map[string]string{}, HTTPPins: map[string]spec.ToolPin{}}
	selections := make([]toolSelection, 0, len(targets)*4)
	for _, t := range targets {
		token := strings.TrimSpace(r.opts.getenv(t.spec.TokenEnv))
		if token == "" {
			return fmt.Errorf("environment variable %s is not set: demo %q needs it for the %q MCP server", t.spec.TokenEnv, r.opts.demo.Name(), t.spec.Name)
		}
		if _, err := r.c.do(ctx, http.MethodPut, "/api/v1/secrets/"+url.PathEscape(t.spec.SecretName), map[string]string{"value": token}, nil); err != nil {
			return fmt.Errorf("write token secret %q: %w", t.spec.SecretName, err)
		}
		r.act("wrote token secret %q from environment variable %s", t.spec.SecretName, t.spec.TokenEnv)

		request := createServerRequest{
			Name:        t.spec.Name,
			Description: t.spec.Description,
			Endpoint:    t.endpoint,
			Transport:   transportStreamableHTTP,
			AuthEnv:     t.spec.SecretName,
			Enabled:     true,
		}
		var created envelope[mcpServerRow]
		if _, err := r.c.do(ctx, http.MethodPost, "/api/v1/mcp-servers", request, &created, http.StatusCreated); err != nil {
			return fmt.Errorf("create MCP server %q: %w", t.spec.Name, err)
		}
		row := created.Data
		if row.ID == "" {
			return fmt.Errorf("create MCP server %q: the response has no id", t.spec.Name)
		}
		if !row.Enabled {
			return fmt.Errorf("create MCP server %q: the server stored it disabled", t.spec.Name)
		}
		if row.AuthEnv != t.spec.SecretName {
			return fmt.Errorf("create MCP server %q: auth_env is %q, want %q", t.spec.Name, row.AuthEnv, t.spec.SecretName)
		}
		r.act("created MCP server %q (id %s)", t.spec.Name, row.ID)

		if _, err := r.c.do(ctx, http.MethodPost, "/api/v1/mcp-servers/"+url.PathEscape(row.ID)+"/validate", nil, nil); err != nil {
			return fmt.Errorf("validate MCP server %q: %w", t.spec.Name, err)
		}
		var detail envelope[mcpServerDetail]
		if _, err := r.c.do(ctx, http.MethodGet, "/api/v1/mcp-servers/"+url.PathEscape(row.ID), nil, &detail); err != nil {
			return fmt.Errorf("read MCP server %q discovery: %w", t.spec.Name, err)
		}
		if detail.Data.ValidationState != "healthy" {
			return fmt.Errorf("MCP server %q did not validate: state %q", t.spec.Name, detail.Data.ValidationState)
		}
		if detail.Data.Discovery == nil {
			return fmt.Errorf("MCP server %q published no discovery after validate", t.spec.Name)
		}
		discovered := make(map[string]discoveryTool, len(detail.Data.Discovery.Tools))
		for _, tool := range detail.Data.Discovery.Tools {
			discovered[tool.RemoteName] = tool
		}
		for _, want := range t.spec.Tools {
			tool, ok := discovered[want]
			if !ok {
				return fmt.Errorf("MCP server %q discovery is missing the %q tool", t.spec.Name, want)
			}
			if tool.SchemaHash == "" {
				return fmt.Errorf("MCP server %q discovery has no schema hash for the %q tool", t.spec.Name, want)
			}
			selections = append(selections, toolSelection{ServerID: row.ID, RemoteName: want, SchemaHash: tool.SchemaHash, Required: true})
		}
		resolution.ServerIDs[t.spec.Logical] = row.ID
		r.report.Created.MCPServers[t.spec.Logical] = row.ID
		r.act("validated MCP server %q: %d tool(s) pinned", t.spec.Name, len(t.spec.Tools))
	}

	httpTools, err := r.listHTTPTools(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]httpToolView, len(httpTools))
	for _, tool := range httpTools {
		byName[tool.Name] = tool
	}
	for _, h := range r.opts.demo.HTTPTools() {
		toolURL, err := joinURL(httpBase, h.Path)
		if err != nil {
			return fmt.Errorf("HTTP tool %q: %w", h.Name, err)
		}
		expected := httpDefinitionRequest{Name: h.Name, Description: h.Description, Method: strings.ToUpper(strings.TrimSpace(h.Method)), URL: toolURL, InputSchema: h.InputSchema}
		current, exists := byName[h.Name]
		if !exists {
			var created envelope[httpToolView]
			_, err := r.c.do(ctx, http.MethodPost, "/api/v1/http-tools", expected, &created, http.StatusCreated)
			var apiErr *apiError
			if err != nil && errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
				// Another instance created the tool between our list and create. Re-read
				// it and continue through the same definition/enabled checks.
				tools, lerr := r.listHTTPTools(ctx)
				if lerr != nil {
					return lerr
				}
				for _, tool := range tools {
					if tool.Name == h.Name {
						current, exists = tool, true
						break
					}
				}
			} else if err != nil {
				return fmt.Errorf("create HTTP tool %q: %w", h.Name, err)
			} else {
				current, exists = created.Data, true
				r.act("created HTTP tool %q (id %s)", h.Name, current.ID)
			}
			if !exists {
				return fmt.Errorf("create HTTP tool %q: the name is taken but the tool cannot be read", h.Name)
			}
		}
		if httpDefinitionDiffers(current.Definition, expected) {
			if !r.opts.yes {
				return fmt.Errorf("HTTP tool %q already exists with a different definition; pass --yes to overwrite it (agents that pinned it will need review)", h.Name)
			}
			var updated envelope[httpToolView]
			if _, err := r.c.do(ctx, http.MethodPut, "/api/v1/http-tools/"+url.PathEscape(current.ID), expected, &updated); err != nil {
				return fmt.Errorf("update HTTP tool %q: %w", h.Name, err)
			}
			current = updated.Data
			r.act("updated HTTP tool %q to the expected definition", h.Name)
		}
		if !current.Enabled {
			var enabled envelope[httpToolView]
			if _, err := r.c.do(ctx, http.MethodPost, "/api/v1/http-tools/"+url.PathEscape(current.ID)+"/enable", nil, &enabled); err != nil {
				return fmt.Errorf("enable HTTP tool %q: %w", h.Name, err)
			}
			current = enabled.Data
			r.act("enabled HTTP tool %q", h.Name)
		}
		id, hash := current.ID, current.Hash
		if id == "" || hash == "" {
			return fmt.Errorf("HTTP tool %q: the server returned no id or hash", h.Name)
		}
		pin := spec.ToolPin{ServerID: httpToolRefPrefix + id, RemoteName: h.Name, SchemaHash: hash, Required: true}
		resolution.HTTPPins[h.Name] = pin
		selections = append(selections, toolSelection{ServerID: pin.ServerID, RemoteName: pin.RemoteName, SchemaHash: pin.SchemaHash, Required: true})
		r.report.Created.HTTPTools[h.Name] = hash
	}

	agent, err := r.opts.demo.Agent(resolution)
	if err != nil {
		return fmt.Errorf("build demo agent: %w", err)
	}
	request := createAgentRequest{Name: agent.Name, Description: agent.Description, Config: agent.Config, MCPTools: selections}
	var created envelope[agentDetail]
	if _, err := r.c.do(ctx, http.MethodPost, "/api/v1/agents", request, &created, http.StatusCreated); err != nil {
		return fmt.Errorf("create demo agent %q: %w", agent.Name, err)
	}
	if created.Data.ID == "" {
		return fmt.Errorf("create demo agent %q: the response has no id", agent.Name)
	}
	r.report.Created.AgentID = created.Data.ID
	r.act("created demo agent %q (id %s)", agent.Name, created.Data.ID)

	var verified envelope[agentDetail]
	if _, err := r.c.do(ctx, http.MethodGet, "/api/v1/agents/"+url.PathEscape(created.Data.ID), nil, &verified); err != nil {
		return fmt.Errorf("verify demo agent %q: %w", agent.Name, err)
	}
	state := verified.Data.Readiness.State
	if state != "ready" && state != "ready_with_tool_warning" {
		reasons := strings.Join(verified.Data.Readiness.Reasons, ", ")
		if reasons == "" {
			reasons = "no reason reported"
		}
		return fmt.Errorf("demo agent %q (id %s) is not callable: readiness %q (%s)", agent.Name, created.Data.ID, state, reasons)
	}
	r.report.Created.Readiness = state
	r.act("verified demo agent %q readiness: %s", agent.Name, state)
	return nil
}

// httpDefinitionDiffers reports whether the stored definition is not the exact
// definition the demo expects. The comparison ignores the server's own
// normalization (default timeout and response cap) and compares the schema as
// JSON, not as bytes.
func httpDefinitionDiffers(current httpDefinitionView, want httpDefinitionRequest) bool {
	if current.Name != want.Name || current.Description != want.Description || current.Method != want.Method || current.URL != want.URL {
		return true
	}
	if len(current.Headers) > 0 || len(current.Pick) > 0 {
		return true
	}
	if len(current.Auth) > 0 && string(current.Auth) != "null" {
		return true
	}
	return !jsonSemanticallyEqual(current.InputSchema, want.InputSchema)
}

func jsonSemanticallyEqual(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}
