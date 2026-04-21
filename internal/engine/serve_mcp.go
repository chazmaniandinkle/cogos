package engine

import "net/http"

// registerMCPRoutes mounts the MCP Streamable HTTP handler at /mcp.
// Explicit method patterns avoid conflicts with the catch-all GET / dashboard route.
//
// The server's agentController (if set via SetAgentController before or
// after registration) is threaded into the MCP tool registry so the
// cog_list_agents / cog_get_agent_state / cog_trigger_agent_loop tools
// have a backing implementation.
func (s *Server) registerMCPRoutes(mux *http.ServeMux) {
	mcpSrv := NewMCPServerWithAgentController(s.cfg, s.nucleus, s.process, s.agentController)
	s.mcpServer = mcpSrv
	h := mcpSrv.Handler()
	mux.Handle("GET /mcp", h)
	mux.Handle("POST /mcp", h)
	mux.Handle("DELETE /mcp", h)
}
