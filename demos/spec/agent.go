package spec

// Stage is one step or call-level config object. It is a plain map so the
// demo definitions stay readable and marshal to the stored JSON unchanged.
type Stage map[string]any

// Step builds a step object with a canvas position.
func Step(id, name string, endCall bool, tools []string, config Stage, x, y float64) Stage {
	s := Stage{"id": id, "name": name, "end_call": endCall, "config": config, "position": Stage{"x": x, "y": y}}
	if len(tools) > 0 {
		s["tools"] = tools
	}
	return s
}

// Refs builds the MCP step tool references for one server.
func Refs(serverID string, names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = serverID + "/" + n
	}
	return out
}

// HTTPRef builds the step reference for one HTTP tool pin.
func HTTPRef(p ToolPin) string { return p.ServerID + "/" + p.RemoteName }

// Handoff is one handoff between steps.
type Handoff struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition"`
}
