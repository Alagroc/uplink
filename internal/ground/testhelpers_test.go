package ground

import (
	"context"
	"encoding/json"

	"github.com/Alagroc/uplink/internal/mcp"
)

// mcpServerFor returns the operator tools as a slice, so tests can invoke a
// handler directly without standing up a transport.
func mcpServerFor(g *Ground) []*mcp.Tool {
	s := mcp.NewServer("test", "test", "")
	g.RegisterOperatorTools(s)
	return s.Tools()
}

// agentToolHandler looks up one agent-side tool handler by name.
func agentToolHandler(g *Ground, name string) func(context.Context, json.RawMessage) (string, error) {
	s := mcp.NewServer("test", "test", "")
	g.RegisterAgentTools(s)
	for _, tool := range s.Tools() {
		if tool.Name == name {
			return tool.Handler
		}
	}
	panic("no agent tool named " + name)
}
