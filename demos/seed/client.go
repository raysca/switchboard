package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// maxResponseBytes bounds one management response. Agent details with a full
// config are the largest; a response over this is an error, never a partial read.
const maxResponseBytes = 4 << 20

// apiError is one failed management request. It carries the method, the path and
// the server's status/code; it never carries a request body, so a token value
// cannot reach an error message or a log line.
type apiError struct {
	Method  string
	Path    string
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	parts := []string{e.Method + " " + e.Path}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
	}
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	return strings.Join(parts, ": ")
}

// client talks to the server's /api/v1 management API with the management key.
type client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

func newClient(baseURL, apiKey string, timeout time.Duration) *client {
	return &client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, hc: &http.Client{Timeout: timeout}}
}

// do sends one request. It returns the status whenever the server answered and an
// error when the transport failed or the status is not in allow (allow empty
// means any 2xx). A 404 that a caller wants to tolerate is listed in allow and
// inspected through the returned status.
func (c *client) do(ctx context.Context, method, path string, body, out any, allow ...int) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("%s %s: encode request: %w", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, fmt.Errorf("%s %s: build request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s %s: request failed: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if len(raw) > maxResponseBytes {
		return resp.StatusCode, fmt.Errorf("%s %s: response exceeds %d bytes", method, path, maxResponseBytes)
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode <= 299
	if len(allow) > 0 {
		ok = slices.Contains(allow, resp.StatusCode)
	}
	if !ok {
		failure := &apiError{Method: method, Path: path, Status: resp.StatusCode}
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			failure.Code, failure.Message = envelope.Error.Code, envelope.Error.Message
		}
		return resp.StatusCode, failure
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// envelope is the management API's success wrapper.
type envelope[T any] struct {
	Data T `json:"data"`
}

// agentRow is one entry of a paginated agent list.
type agentRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Archived bool   `json:"archived"`
}

type readinessView struct {
	State      string   `json:"state"`
	Reasons    []string `json:"reasons"`
	MissingEnv []string `json:"missing_env,omitempty"`
}

type agentDetail struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Archived  bool          `json:"archived"`
	Readiness readinessView `json:"readiness"`
}

type toolSelection struct {
	ServerID   string `json:"mcp_server_id"`
	RemoteName string `json:"remote_name"`
	SchemaHash string `json:"schema_hash"`
	Required   bool   `json:"required"`
}

type createAgentRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Config      json.RawMessage `json:"config"`
	MCPTools    []toolSelection `json:"mcp_tools"`
}

// mcpServerRow is what the MCP server list and detail responses share.
type mcpServerRow struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Endpoint          string `json:"endpoint"`
	Transport         string `json:"transport"`
	AuthEnv           string `json:"auth_env"`
	Enabled           bool   `json:"enabled"`
	PendingDeletion   bool   `json:"pending_deletion"`
	ValidationState   string `json:"validation_state"`
	LatestDiscoveryID string `json:"latest_discovery_id"`
}

type discoveryTool struct {
	RemoteName  string          `json:"remote_name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	SchemaHash  string          `json:"schema_hash"`
}

type discoveryView struct {
	Tools []discoveryTool `json:"tools"`
}

type mcpServerDetail struct {
	mcpServerRow
	Discovery *discoveryView `json:"discovery,omitempty"`
}

type createServerRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Endpoint    string `json:"endpoint"`
	Transport   string `json:"transport"`
	AuthEnv     string `json:"auth_env"`
	Enabled     bool   `json:"enabled"`
}

type detachResult struct {
	Pending bool          `json:"pending"`
	Deleted bool          `json:"deleted"`
	Server  *mcpServerRow `json:"server,omitempty"`
}

// httpDefinitionRequest is the exact part of an HTTP tool definition the demo
// owns. The server normalizes defaults (timeout, response cap) itself.
type httpDefinitionRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Method      string          `json:"method"`
	URL         string          `json:"url"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// httpDefinitionView is the normalized definition the server reports.
type httpDefinitionView struct {
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Method           string            `json:"method"`
	URL              string            `json:"url"`
	Headers          map[string]string `json:"headers,omitempty"`
	Auth             json.RawMessage   `json:"auth,omitempty"`
	InputSchema      json.RawMessage   `json:"input_schema"`
	TimeoutMS        int               `json:"timeout_ms"`
	MaxResponseBytes int               `json:"max_response_bytes"`
	Pick             []string          `json:"response_pick,omitempty"`
}

type httpToolView struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Enabled    bool               `json:"enabled"`
	Hash       string             `json:"hash"`
	Definition httpDefinitionView `json:"definition"`
}

type secretListView struct {
	StoreEnabled bool `json:"store_enabled"`
	Secrets      []struct {
		Name string `json:"name"`
	} `json:"secrets"`
}
