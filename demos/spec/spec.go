// Package spec is the importable contract between demo definitions and the seed
// client. Demo packages import spec; the registry and seed import spec. Nothing
// in this package knows about the server runtime.
package spec

import "encoding/json"

// MCPServerSpec describes one MCP server a demo needs.
type MCPServerSpec struct {
	// Logical is the name used by --mcp <logical>=<url> and Agent resolutions.
	Logical string
	// Name is the server name stored in the management API.
	Name        string
	Description string
	// EndpointPath is appended to the base URL (normally "/mcp").
	EndpointPath string
	// TokenEnv is the environment variable holding the bearer token value.
	TokenEnv string
	// SecretName is the stored-secret name written to the server.
	SecretName string
	// Tools are the remote tool names the demo pins and expects on discovery.
	Tools []string
}

// HTTPToolSpec describes one HTTP endpoint tool a demo needs.
type HTTPToolSpec struct {
	Name        string
	Description string
	Method      string
	Path        string
	InputSchema json.RawMessage
}

// ToolPin is one tool selection pinned on an agent version.
type ToolPin struct {
	ServerID   string
	RemoteName string
	SchemaHash string
	Required   bool
}

// Resolution is what the seed client discovered for a demo.
type Resolution struct {
	// ServerIDs maps a logical MCP server name to its created id.
	ServerIDs map[string]string
	// HTTPPins maps an HTTP tool name to its current definition pin.
	HTTPPins map[string]ToolPin
}

// AgentSpec is the agent a demo creates after its tools exist.
type AgentSpec struct {
	Name        string
	Description string
	Config      json.RawMessage
}

// Demo is one installable demo definition.
type Demo interface {
	Name() string
	MCPServers() []MCPServerSpec
	HTTPTools() []HTTPToolSpec
	Agent(r Resolution) (AgentSpec, error)
}
