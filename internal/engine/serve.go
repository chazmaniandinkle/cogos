// serve.go — CogOS v3 HTTP API
//
// Core endpoints:
//
//	GET  /health                           — liveness + readiness probe
//	GET  /v1/context                       — current attentional field (debug)
//	GET  /v1/resolve                       — resolve a cog:// URI to a filesystem path
//	POST /v1/chat/completions              — OpenAI-compatible chat (streaming + non-streaming)
//	POST /v1/messages                      — Anthropic Messages-compatible chat
//	POST /v1/context/foveated              — foveated context assembly for Claude Code hook
//	GET  /v1/proprioceptive                — last 50 proprioceptive log entries + light cone status
//	GET  /v1/ledger                        — query the hash-chained event ledger
//	GET  /v1/lightcone                     — light cone metadata (placeholder)
//	GET  /v1/kernel-log                    — tail kernel slog (diagnostic text) JSONL sink; filter by level/substring/time
//
// Constellation / attention endpoints (Phase 3, see serve_attention.go):
//
//	POST /v1/attention                     — emit attention signal
//	GET  /v1/constellation/fovea           — current fovea state
//	GET  /v1/constellation/adjacent?uri=… — adjacent nodes by attentional proximity
//
// Channel-session forwarder (ADR-082 Wave 2, see serve_sessions_channel.go):
//
//	POST /v1/channel-sessions/register             — kernel mints session_id
//	                                                  and forwards to mod3;
//	                                                  returns merged response
//	POST /v1/channel-sessions/{id}/deregister      — proxy to mod3, drop record
//	GET  /v1/channel-sessions                      — kernel view + mod3 list
//	GET  /v1/channel-sessions/{id}                 — single-session detail
//
// The chat endpoint routes through the inference Router when one is set,
// otherwise returns 501.
package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// Server wraps the HTTP server and its dependencies.
type Server struct {
	cfg             *Config
	nucleus         *Nucleus
	process         *Process
	router          Router // nil until SetRouter is called
	srv             *http.Server
	debug           debugStore      // captures last request pipeline state
	attentionLog    *attentionLog   // per-server log (avoids global write race)
	agentController AgentController // nil until SetAgentController is called
	mcpServer       *MCPServer      // so SetAgentController can propagate to tools

	// Track 5 Phase 3 surface — per-bus event store, SSE broker, and
	// consumer cursor registry. Scoped to the server so tests can create
	// isolated instances.
	busSessions  *BusSessionManager
	busBroker    *BusEventBroker
	busConsumers *ConsumerRegistry
	sessions     *SessionContextStore

	// Kernel-native session-management registries (hybrid design —
	// cog://mem/semantic/surveys/2026-04-21-consolidation/
	// agent-P-session-management-evaluation). The bus is ground truth;
	// these are derived views rebuilt from bus replay at startup.
	sessionRegistry *SessionRegistry
	handoffRegistry *HandoffRegistry

	// ADR-082 Wave 2: kernel-owned identity registry for channel-participant
	// sessions. The kernel mints session_id, mod3 stores per-channel state
	// keyed on the kernel-issued ID. Distinct from sessionRegistry above,
	// which enforces strict 3-component hyphen IDs for the agent/handoff
	// protocol. See serve_sessions_channel.go for the full rationale.
	channelSessionRegistry *ChannelSessionRegistry

	// mod3Client is the HTTP client used to forward channel-session calls
	// to mod3. Nil in production (falls back to the package-level
	// mod3HTTPClient); tests set this to an httptest-backed client.
	mod3Client *http.Client

	// httpRoutes is the manifest-introspection registry. Every route added
	// via s.route / s.routeH appends here; /v1/manifest serialises this
	// slice. Populated at startup only — reads are lock-free because the
	// slice is frozen by the time Start returns.
	httpRoutes []routeMeta
}

// NewServer constructs a Server bound to the configured port.
func NewServer(cfg *Config, nucleus *Nucleus, process *Process) *Server {
	s := &Server{cfg: cfg, nucleus: nucleus, process: process}

	// Phase 3 bus/session surface. Managers are always instantiated so
	// handlers don't need nil-safety for the common case; tests can
	// override via the exported fields if they want an isolated fixture.
	s.busSessions = NewBusSessionManager(cfg.WorkspaceRoot)
	s.busBroker = NewBusEventBroker()
	s.busConsumers = NewConsumerRegistry(
		// Match root's persistence path: .cog/run/bus/{bus_id}.cursors.jsonl
		filepath.Join(cfg.WorkspaceRoot, ".cog", "run", "bus"),
	)
	s.sessions = NewSessionContextStore()

	// Kernel-native session + handoff registries. Replay from bus is done
	// below, after the bus manager + its handlers are wired up, so that
	// the warm cache is ready before any HTTP request lands.
	s.sessionRegistry = NewSessionRegistry()
	s.handoffRegistry = NewHandoffRegistry()

	// ADR-082 Wave 2 kernel-owned channel-session identity.
	s.channelSessionRegistry = NewChannelSessionRegistry()

	mux := http.NewServeMux()
	s.route(mux, "GET /", handleDashboard)
	s.route(mux, "GET /canvas", handleCanvas)
	s.route(mux, "GET /health", s.handleHealth)
	s.route(mux, "GET /v1/context", s.handleContext)
	s.route(mux, "GET /v1/resolve", s.handleResolve)
	s.route(mux, "GET /v1/cogdoc/read", s.handleCogDocRead)
	s.route(mux, "GET /v1/debug/last", s.handleDebugLast)
	s.route(mux, "GET /v1/debug/context", s.handleDebugContext)
	s.route(mux, "POST /v1/chat/completions", s.handleChat)
	s.route(mux, "POST /v1/messages", s.handleAnthropicMessages)
	s.route(mux, "GET /v1/proprioceptive", s.handleProprioceptive)
	s.route(mux, "GET /v1/ledger", s.handleLedger)
	s.route(mux, "GET /v1/traces", s.handleTraces)
	s.route(mux, "GET /v1/lightcone", s.handleLightCone)
	s.route(mux, "POST /v1/context/foveated", s.handleFoveatedContext)
	s.route(mux, "GET /v1/kernel-log", s.handleKernelLog)
	s.route(mux, "GET /v1/tool-calls", s.handleToolCalls)
	s.route(mux, "GET /v1/conversation", s.handleConversation)
	s.route(mux, "GET /v1/manifest", s.handleManifest)

	// Constellation / attention endpoints (Phase 3)
	s.registerAttentionRoutes(mux)

	// Block sync endpoints (Phase 3 block sync protocol)
	s.registerBlockRoutes(mux)
	s.registerCompatRoutes(mux)
	s.registerEventBusRoutes(mux)
	s.registerMCPRoutes(mux)
	s.registerConfigRoutes(mux)

	// Track 5 Phase 3: /v1/bus/* and /v1/sessions routes.
	s.registerBusRoutes(mux)

	// Kernel-native session & handoff management routes — the hybrid
	// design's invariance layer. Registered AFTER registerBusRoutes so
	// the specific patterns (POST /v1/sessions/register, etc.) coexist
	// cleanly with the pre-existing GET /v1/sessions[/{id}] surface.
	s.registerSessionMgmtRoutes(mux)

	// ADR-082 Wave 2: kernel-side channel-session forwarder. The four
	// /v1/channel-sessions/* routes mint session_ids, record identity
	// locally, and forward to mod3 at cfg.Mod3URL. Namespaced under
	// /v1/channel-sessions/* to coexist with the agent-session surface
	// above (incompatible session_id formats — see serve_sessions_channel.go).
	s.registerChannelSessionRoutes(mux)

	// Replay bus_sessions + bus_handoffs into the in-memory registries so
	// the kernel starts with an accurate derived view. Bus is authoritative
	// either way; this just warms the read path.
	_ = ReplaySessionRegistry(s.busSessions, s.sessionRegistry)
	_ = ReplayHandoffRegistry(s.busSessions, s.handoffRegistry)

	// Resolve the bind address. Default stays 127.0.0.1 (loopback-only);
	// callers may override via Config.BindAddr to listen on all interfaces
	// ("0.0.0.0") for pod/LAN/Tailnet deployments.
	bindAddr := cfg.BindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	s.srv = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", bindAddr, cfg.Port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second, // 5 min — streaming responses can be long
		IdleTimeout:  120 * time.Second,
	}
	return s
}

// SetRouter wires an inference Router into the server.
func (s *Server) SetRouter(r Router) {
	s.router = r
}

// SetAgentController wires a live AgentController into the server so the
// cog_list_agents / cog_get_agent_state / cog_trigger_agent_loop MCP
// tools have a backing implementation. Callers outside engine (like the
// root-package serveServer) can build the controller and pass it here.
// Safe to call post-construction: the MCP tool registry resolves the
// controller at call time.
func (s *Server) SetAgentController(ctrl AgentController) {
	s.agentController = ctrl
	if s.mcpServer != nil {
		s.mcpServer.SetAgentController(ctrl)
	}
}

// Start begins serving. It blocks until the server stops.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.srv.Addr, err)
	}
	slog.Info("server: listening", "addr", s.srv.Addr, "bind", s.cfg.BindAddr)
	return s.srv.Serve(ln)
}

// Shutdown gracefully drains the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

// Handler returns the HTTP handler, useful for httptest.NewServer in tests.
func (s *Server) Handler() http.Handler {
	return s.srv.Handler
}

// handleHealth is the liveness/readiness probe.
//
//	200 → healthy
//	503 → nucleus not loaded or process not running
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK

	if s.nucleus == nil {
		status = "nucleus_missing"
		code = http.StatusServiceUnavailable
	}

	identity := ""
	if s.nucleus != nil {
		identity = s.nucleus.Name
	}
	trust := s.process.TrustSnapshot()

	resp := map[string]interface{}{
		"status":   status,
		"version":  Version,
		"state":    s.process.State().String(),
		"identity": identity,
		"node_id":  s.process.NodeID,
		"trust": map[string]interface{}{
			"score":       trust.LocalScore,
			"scope":       "local",
			"fingerprint": s.process.Fingerprint(),
		},
		"workspace": s.cfg.WorkspaceRoot,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}

	if nh := s.process.NodeHealth(); nh != nil {
		if summary := nh.Summary(); len(summary) > 0 {
			resp["node"] = summary
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleContext returns the current attentional field (top-20 fovea).
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	fovea := s.process.Field().Fovea(20)

	type entry struct {
		Path  string  `json:"path"`
		Score float64 `json:"score"`
	}
	entries := make([]entry, len(fovea))
	for i, fs := range fovea {
		entries[i] = entry(fs)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"nucleus":      s.nucleus.Name,
		"state":        s.process.State().String(),
		"field_size":   s.process.Field().Len(),
		"last_updated": s.process.Field().LastUpdated().Format(time.RFC3339),
		"fovea":        entries,
	})
}

// handleResolve resolves a cog:// URI to a filesystem path.
//
// GET /v1/resolve?uri=cog://mem/semantic/foo.cog.md
//
//	200 → { uri, path, fragment, exists }
//	400 → { error }
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "uri parameter required"})
		return
	}

	res, err := ResolveURI(s.cfg.WorkspaceRoot, uri)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	_, statErr := os.Stat(res.Path)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"uri":      uri,
		"path":     res.Path,
		"fragment": res.Fragment,
		"exists":   statErr == nil,
	})
}

// handleCogDocRead resolves a cog:// URI and returns the file content as text.
//
//	GET /v1/cogdoc/read?uri=cog://mem/semantic/insights/foo.md
//	200 → { uri, path, content, exists }
func (s *Server) handleCogDocRead(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.Query().Get("uri")
	if uri == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "uri parameter required"})
		return
	}

	res, err := ResolveURI(s.cfg.WorkspaceRoot, uri)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	content, readErr := os.ReadFile(res.Path)
	exists := readErr == nil

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"uri":    uri,
		"path":   res.Path,
		"exists": exists,
	}
	if exists {
		resp["content"] = string(content)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// ── OpenAI-compatible wire types ─────────────────────────────────────────────

type oaiChatRequest struct {
	Model               string              `json:"model"`
	Messages            []oaiMessage        `json:"messages"`
	Stream              bool                `json:"stream"`
	Temperature         *float64            `json:"temperature,omitempty"`
	MaxTokens           int                 `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                 `json:"max_completion_tokens,omitempty"`
	TopP                *float64            `json:"top_p,omitempty"`
	Stop                []string            `json:"stop,omitempty"`
	Tools               []oaiToolDefinition `json:"tools,omitempty"`
	ToolChoice          json.RawMessage     `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool               `json:"parallel_tool_calls,omitempty"`
	FrequencyPenalty    *float64            `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64            `json:"presence_penalty,omitempty"`
	Seed                *int                `json:"seed,omitempty"`
	User                string              `json:"user,omitempty"`
	N                   *int                `json:"n,omitempty"`
	StreamOptions       *oaiStreamOpts      `json:"stream_options,omitempty"`
}

// oaiStreamOpts carries OpenAI stream_options (e.g. include_usage).
type oaiStreamOpts struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// oaiToolDefinition is the OpenAI-format tool envelope: {"type":"function","function":{...}}.
type oaiToolDefinition struct {
	Type     string          `json:"type"`
	Function oaiToolFunction `json:"function"`
}

// oaiToolFunction carries the tool name, description, and JSON Schema parameters.
type oaiToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

// oaiToolCall is the OpenAI-format tool call in a response message.
type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiToolCallFunc `json:"function"`
}

// oaiToolCallFunc carries the function name and stringified arguments.
type oaiToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// oaiStreamToolCall is a tool call delta in a streaming response.
type oaiStreamToolCall struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function *oaiToolCallFunc `json:"function,omitempty"`
}

type oaiMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

// extractContent normalises the OpenAI "content" field which may arrive as
// either a plain JSON string or an array of content-parts (the multi-part
// format used by Discord gateway and other clients):
//
//	"hello"                                   → "hello"
//	[{"type":"text","text":"hello"}]           → "hello"
func extractContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Fast path: plain string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Slow path: array of content parts.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		// Unrecognised shape — return the raw bytes as-is so nothing is lost.
		return string(raw)
	}

	var out string
	for _, p := range parts {
		if p.Type == "text" {
			out += p.Text
		}
	}
	return out
}

// oaiContentPart represents a single element in the OpenAI multi-part content array.
type oaiContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

// oaiImageURL carries the URL (typically a data: base64 URI) for an image content part.
type oaiImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// extractContentParts normalises the OpenAI "content" field into structured
// parts, preserving both text and image_url entries. This is used instead of
// extractContent when the caller needs to forward image data to providers.
func extractContentParts(raw json.RawMessage) []oaiContentPart {
	if len(raw) == 0 {
		return nil
	}

	// Fast path: plain string → single text part.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []oaiContentPart{{Type: "text", Text: s}}
	}

	// Slow path: array of content parts.
	var parts []oaiContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		// Unrecognised shape — wrap raw bytes as text so nothing is lost.
		return []oaiContentPart{{Type: "text", Text: string(raw)}}
	}
	return parts
}

// mustMarshalString wraps a Go string as a JSON-encoded string suitable for
// json.RawMessage (i.e. it adds the surrounding quotes and escapes).
func mustMarshalString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return json.RawMessage(b)
}

type oaiChoice struct {
	Index        int         `json:"index"`
	Message      *oaiMessage `json:"message,omitempty"`
	Delta        *oaiMessage `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaiChatResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage,omitempty"`
}

// handleChat is the OpenAI-compatible /v1/chat/completions endpoint.
// Routes through the inference Router when set; returns 501 otherwise.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	ctx, span := otel.Tracer("cogos-v3").Start(r.Context(), "chat.request")
	defer span.End()
	r = r.WithContext(ctx)

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MB limit
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req oaiChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "parse body: "+err.Error(), http.StatusBadRequest)
		return
	}

	block := NormalizeOpenAIRequest(&req, body, "http")
	block.SessionID = s.process.SessionID()
	if s.nucleus != nil {
		block.TargetIdentity = s.nucleus.Name
	}
	block.WorkspaceID = filepath.Base(s.cfg.WorkspaceRoot)
	s.process.RecordBlock(block)

	clientMsgs := block.Messages

	// Resolve any pending client-ownership tool calls whose results are
	// arriving on this turn. Each role=tool message carries a tool_call_id
	// that matches a previously-forwarded tool.call; emitting the paired
	// tool.result closes the ledger pair.
	for _, msg := range clientMsgs {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			s.process.resolvePendingToolCall(msg.ToolCallID, msg.Content)
		}
	}

	// Extract the user's latest message as the query for relevance scoring.
	query := ""
	for i := len(clientMsgs) - 1; i >= 0; i-- {
		if clientMsgs[i].Role == "user" {
			query = clientMsgs[i].Content
			break
		}
	}

	// Notify the process of the incoming interaction.
	s.process.Send(NewGateEventFromBlock(block, "user.message", query))

	if s.router == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "not_implemented",
				"message": "no inference router configured; run with a providers.yaml",
			},
		})
		return
	}

	// Assemble foveated context — engine owns the full window.
	// It decomposes client messages, scores them alongside CogDocs,
	// and manages the budget including conversation history.

	// Resolve max tokens: prefer max_completion_tokens (OpenAI v2 field,
	// sent by Zed and newer clients) over legacy max_tokens.
	maxToks := req.MaxTokens
	if req.MaxCompletionTokens > 0 {
		maxToks = req.MaxCompletionTokens
	}

	creq := &CompletionRequest{
		MaxTokens:     maxToks,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		Stop:          req.Stop,
		InteractionID: block.ID,
		Metadata: RequestMetadata{
			RequestID:    uuid.New().String(),
			ProcessState: "active", // chat requests are always active interactions
			Priority:     PriorityNormal,
			Source:       "http",
		},
	}

	// Convert OpenAI-format tool definitions to internal ToolDefinition.
	if len(req.Tools) > 0 {
		creq.Tools = make([]ToolDefinition, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "function" {
				continue
			}
			creq.Tools = append(creq.Tools, ToolDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}

	// Convert tool_choice: OpenAI sends either a string ("auto"/"none"/"required")
	// or an object {"type":"function","function":{"name":"..."}}.
	if len(req.ToolChoice) > 0 {
		var tcStr string
		if err := json.Unmarshal(req.ToolChoice, &tcStr); err == nil {
			creq.ToolChoice = tcStr
		} else {
			var tcObj struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.ToolChoice, &tcObj); err == nil && tcObj.Function.Name != "" {
				creq.ToolChoice = tcObj.Function.Name
			}
		}
	}

	// Map OpenClaw model names to provider routing.
	// "claude", "codex", "ollama" are provider aliases, not model names.
	switch req.Model {
	case "", "local":
		// use default routing
	case "claude":
		creq.Metadata.PreferProvider = "claude-code"
	case "codex":
		creq.Metadata.PreferProvider = "codex"
	case "ollama":
		creq.Metadata.PreferProvider = "ollama"
	default:
		// Pass through as model override (e.g. "opus", "haiku", "gpt-5.4")
		creq.ModelOverride = req.Model
	}

	var pkg *ContextPackage
	conversationTurnsIn := 0
	for _, m := range clientMsgs {
		if m.Role != "system" {
			conversationTurnsIn++
		}
	}

	if p, err := s.process.AssembleContext(query, clientMsgs, 0,
		WithContext(r.Context()),
		WithConversationID(creq.Metadata.RequestID),
		WithManifestMode(true),
	); err != nil {
		slog.Warn("chat: context assembly failed", "err", err)
		creq.Messages = clientMsgs
	} else {
		pkg = p
		systemPrompt, managedMsgs := pkg.FormatForProvider()
		creq.SystemPrompt = systemPrompt
		creq.Messages = managedMsgs

		// Record metrics + span attributes.
		span.SetAttributes(
			attribute.Int("cogos.context.total_tokens", pkg.TotalTokens),
			attribute.Int("cogos.context.docs_injected", len(pkg.FovealDocs)),
			attribute.Int("cogos.context.conv_turns_kept", len(pkg.Conversation)),
			attribute.Int("cogos.context.conv_turns_in", conversationTurnsIn),
		)
		if instruments.ContextTokens != nil {
			instruments.ContextTokens.Record(ctx, int64(pkg.TotalTokens))
		}
		if instruments.DocsInjected != nil {
			instruments.DocsInjected.Record(ctx, int64(len(pkg.FovealDocs)))
		}
		evicted := conversationTurnsIn - len(pkg.Conversation)
		if evicted > 0 && instruments.TurnsEvicted != nil {
			instruments.TurnsEvicted.Add(ctx, int64(evicted))
		}

		if len(pkg.InjectedPaths) > 0 {
			slog.Info("chat: context injected",
				"docs", len(pkg.InjectedPaths),
				"conv_turns", len(pkg.Conversation),
				"tokens", pkg.TotalTokens,
			)
		}
	}

	provider, _, err := s.router.Route(r.Context(), creq)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": sanitizeErrorMessage(err.Error()),
				"type":    "server_error",
				"param":   nil,
				"code":    nil,
			},
		})
		return
	}

	respID := "chatcmpl-" + uuid.New().String()
	model := provider.Name()
	if req.Model != "" && req.Model != "local" {
		model = req.Model
	}

	span.SetAttributes(
		attribute.String("cogos.provider", provider.Name()),
		attribute.String("cogos.model", model),
	)
	if instruments.ChatRequests != nil {
		instruments.ChatRequests.Add(ctx, 1)
	}

	// Prepare the turn record. Fully populated by the provider path (complete/stream)
	// below, then persisted via RecordTurn once the response is on its way to the client.
	turn := &TurnRecord{
		TurnID:    uuid.New().String(),
		TurnIndex: NextTurnIndex(s.cfg.WorkspaceRoot, block.SessionID),
		SessionID: block.SessionID,
		Timestamp: time.Now().UTC(),
		Prompt:    query,
		Provider:  provider.Name(),
		Model:     model,
		BlockID:   block.ID,
	}

	inferStart := time.Now()
	if req.Stream {
		s.streamChat(w, r.Context(), creq, provider, respID, model, req.StreamOptions, turn)
	} else {
		s.completeChat(w, r.Context(), creq, provider, respID, model, turn)
	}

	inferMs := float64(time.Since(inferStart).Milliseconds())
	span.SetAttributes(attribute.Float64("cogos.inference.latency_ms", inferMs))
	if instruments.InferenceLatency != nil {
		instruments.InferenceLatency.Record(ctx, inferMs)
	}

	// Persist the turn (ledger event + sidecar). Closes cogos#20 by
	// capturing the full prompt/response pair, which RecordBlock drops.
	turn.DurationMs = time.Since(inferStart).Milliseconds()
	if err := s.process.RecordTurn(turn); err != nil {
		slog.Warn("chat: RecordTurn failed", "err", err, "session", turn.SessionID)
	}

	// Capture debug snapshot (best-effort, non-blocking).
	go func() {
		snap := captureDebugSnapshot(
			clientMsgs, query, req.Model, pkg, conversationTurnsIn,
			provider.Name(), model, 0, time.Since(inferStart),
		)
		s.debug.Store(snap)
	}()
}

// completeChat handles non-streaming chat completions.
// The optional turn record is populated with the response text, usage, and
// any tool-call traces so the caller can persist the full turn via RecordTurn.
func (s *Server) completeChat(w http.ResponseWriter, ctx context.Context, req *CompletionRequest,
	provider Provider, respID, model string, turn *TurnRecord) {

	resp, err := provider.Complete(ctx, req)
	if err != nil {
		slog.Warn("chat: complete error", "err", err)
		if turn != nil {
			turn.Status = "error"
			turn.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": sanitizeErrorMessage(err.Error()),
				"type":    "server_error",
				"param":   nil,
				"code":    nil,
			},
		})
		return
	}
	if turn != nil {
		turn.Response = resp.Content
		turn.Usage = TurnUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
		// Non-kernel tool calls returned to the client become transcript
		// entries with empty result (the client will execute them).
		if len(resp.ToolCalls) > 0 {
			for _, tc := range resp.ToolCalls {
				turn.ToolCalls = append(turn.ToolCalls, ToolCallRecord{
					ID:        tc.ID,
					Name:      tc.Name,
					Arguments: tc.Arguments,
				})
			}
		}
	}

	msg := &oaiMessage{Role: "assistant", Content: mustMarshalString(resp.Content)}
	finishReason := mapStopReasonToOpenAI(resp.StopReason)
	if finishReason == "" {
		finishReason = "stop"
	}

	// Wrap tool calls in the OpenAI response format.
	if len(resp.ToolCalls) > 0 {
		finishReason = "tool_calls"
		// OpenAI spec: tool-call-only messages must have "content": null, not "".
		if resp.Content == "" {
			msg.Content = json.RawMessage("null")
		}
		calls := make([]oaiToolCall, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			calls[i] = oaiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: oaiToolCallFunc{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			}
			// Observability: emit tool.call for every client-ownership tool
			// the server is about to forward, and register a pending entry
			// so the next-turn role=tool message resolves to a tool.result.
			s.process.emitToolCall(ToolCallEvent{
				CallID:    tc.ID,
				ToolName:  tc.Name,
				Arguments: json.RawMessage(tc.Arguments),
				Source:    ToolSourceOpenAI,
				Ownership: ToolOwnershipClient,
				Provider:  provider.Name(),
				SessionID: s.process.SessionID(),
			})
			s.process.registerPendingToolCall(tc.ID, tc.Name, ToolSourceOpenAI, 0)
		}
		raw, _ := json.Marshal(calls)
		msg.ToolCalls = json.RawMessage(raw)
	}

	oai := oaiChatResponse{
		ID:      respID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []oaiChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}},
		Usage: &oaiUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(oai)
}

// streamChat handles streaming chat completions via SSE.
// The optional turn record accumulates the response text (and usage from
// the final chunk) so the caller can persist the full turn via RecordTurn.
func (s *Server) streamChat(w http.ResponseWriter, ctx context.Context, req *CompletionRequest,
	provider Provider, respID, model string, streamOpts *oaiStreamOpts, turn *TurnRecord) {

	chunks, err := provider.Stream(ctx, req)
	if err != nil {
		slog.Warn("chat: stream error", "err", err)
		if turn != nil {
			turn.Status = "error"
			turn.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": sanitizeErrorMessage(err.Error()),
				"type":    "server_error",
				"param":   nil,
				"code":    nil,
			},
		})
		return
	}
	var respBuf strings.Builder

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, canFlush := w.(http.Flusher)
	bw := bufio.NewWriter(w)

	flush := func() {
		_ = bw.Flush()
		if canFlush {
			flusher.Flush()
		}
	}

	writeSSE := func(data []byte) {
		_, _ = fmt.Fprintf(bw, "data: %s\n\n", data)
		flush()
	}

	// Track whether any tool calls were streamed (affects finish_reason).
	sawToolCall := false

	for sc := range chunks {
		if sc.Error != nil {
			slog.Warn("chat: stream chunk error", "err", sc.Error)
			if turn != nil {
				turn.Status = "error"
				turn.Error = sc.Error.Error()
			}
			break
		}

		if sc.Done {
			// Final chunk: emit finish_reason.
			// Prefer the provider-reported stop reason, falling back to heuristic.
			finishReason := mapStopReasonToOpenAI(sc.StopReason)
			if finishReason == "" {
				finishReason = "stop"
				if sawToolCall {
					finishReason = "tool_calls"
				}
			}
			// Populate the turn record with accumulated response + final usage.
			if turn != nil {
				turn.Response = respBuf.String()
				if sc.Usage != nil {
					turn.Usage = TurnUsage{
						InputTokens:  sc.Usage.InputTokens,
						OutputTokens: sc.Usage.OutputTokens,
						TotalTokens:  sc.Usage.InputTokens + sc.Usage.OutputTokens,
					}
				}
			}
			data := oaiChatResponse{
				ID:      respID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []oaiChoice{{Index: 0, FinishReason: &finishReason}},
			}
			// When stream_options.include_usage is set, include usage in the final chunk.
			if streamOpts != nil && streamOpts.IncludeUsage && sc.Usage != nil {
				data.Usage = &oaiUsage{
					PromptTokens:     sc.Usage.InputTokens,
					CompletionTokens: sc.Usage.OutputTokens,
					TotalTokens:      sc.Usage.InputTokens + sc.Usage.OutputTokens,
				}
			}
			b, _ := json.Marshal(data)
			writeSSE(b)
			break
		}

		// Tool call delta — wrap in OpenAI streaming tool_calls format.
		if sc.ToolCallDelta != nil {
			sawToolCall = true
			tc := oaiStreamToolCall{
				Index: sc.ToolCallDelta.Index,
			}
			if sc.ToolCallDelta.ID != "" {
				tc.ID = sc.ToolCallDelta.ID
				tc.Type = "function"
				// Emit tool.call on the first delta that carries an ID.
				// Arguments arrive incrementally in later deltas; for the
				// ledger row we record what we have at announcement time
				// (typically just the name — arguments accumulate client-
				// side). The paired tool.result will still fire on the
				// follow-up role=tool message.
				s.process.emitToolCall(ToolCallEvent{
					CallID:    sc.ToolCallDelta.ID,
					ToolName:  sc.ToolCallDelta.Name,
					Source:    ToolSourceOpenAI,
					Ownership: ToolOwnershipClient,
					Provider:  provider.Name(),
					SessionID: s.process.SessionID(),
				})
				s.process.registerPendingToolCall(sc.ToolCallDelta.ID, sc.ToolCallDelta.Name, ToolSourceOpenAI, 0)
			}
			// Always create Function — OpenAI spec requires it on every tool call
			// delta, even the initial chunk where only Name is set and Arguments
			// is empty. Omitting Function causes clients to see function: null.
			tc.Function = &oaiToolCallFunc{
				Name:      sc.ToolCallDelta.Name,
				Arguments: sc.ToolCallDelta.ArgsDelta,
			}
			tcRaw, _ := json.Marshal([]oaiStreamToolCall{tc})
			// Content left nil → serialises as "content": null (OpenAI spec for tool-call-only deltas).
			delta := &oaiMessage{Role: "assistant", ToolCalls: json.RawMessage(tcRaw)}
			data := oaiChatResponse{
				ID:      respID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []oaiChoice{{Index: 0, Delta: delta}},
			}
			b, _ := json.Marshal(data)
			writeSSE(b)
			continue
		}

		// Text delta.
		if sc.Delta != "" {
			if turn != nil {
				respBuf.WriteString(sc.Delta)
			}
			delta := &oaiMessage{Role: "assistant", Content: mustMarshalString(sc.Delta)}
			data := oaiChatResponse{
				ID:      respID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []oaiChoice{{Index: 0, Delta: delta}},
			}
			b, _ := json.Marshal(data)
			writeSSE(b)
		}
	}
	// Ensure the turn record picks up the response text even when the
	// stream never sent a Done chunk (error mid-stream, client disconnect).
	if turn != nil && turn.Response == "" {
		turn.Response = respBuf.String()
	}
	_, _ = fmt.Fprint(bw, "data: [DONE]\n\n")
	flush()
}

// handleToolCalls is the HTTP companion to cog_read_tool_calls.
//
// GET /v1/tool-calls
//
//	?session_id=&tool_name=&status=&source=&ownership=&call_id=
//	&since=&until=&limit=&order=
//	&include_args=&include_output=
//
// Returns the same stitched call+result rows as the MCP tool. Missing or
// malformed query params error with 400; empty filter set returns the
// default-limit most-recent rows.
func (s *Server) handleToolCalls(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	q := r.URL.Query()
	query := ToolCallQuery{
		SessionID:     q.Get("session_id"),
		ToolName:      q.Get("tool_name"),
		Status:        q.Get("status"),
		Source:        q.Get("source"),
		Ownership:     q.Get("ownership"),
		CallID:        q.Get("call_id"),
		Order:         q.Get("order"),
		IncludeArgs:   boolQueryParam(q.Get("include_args")),
		IncludeOutput: boolQueryParam(q.Get("include_output")),
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := parseIntQuery(raw)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid limit"})
			return
		}
		query.Limit = n
	}
	if raw := q.Get("since"); raw != "" {
		ts, err := parseTimeOrDuration(raw)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid since: " + err.Error()})
			return
		}
		query.Since = ts
	}
	if raw := q.Get("until"); raw != "" {
		ts, err := parseTimeOrDuration(raw)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid until: " + err.Error()})
			return
		}
		query.Until = ts
	}

	result, err := QueryToolCalls(s.cfg.WorkspaceRoot, query)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

// boolQueryParam accepts "true", "1", "yes" (case-insensitive) as true.
func boolQueryParam(raw string) bool {
	switch regexp.MustCompile(`^(true|1|yes|on)$`).MatchString(raw) {
	case true:
		return true
	default:
		return false
	}
}

// parseIntQuery returns the int value of a query string param, or an error.
func parseIntQuery(raw string) (int, error) {
	var n int
	_, err := fmt.Sscanf(raw, "%d", &n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// handleDebugLast returns the full pipeline snapshot from the most recent chat request.
func (s *Server) handleDebugLast(w http.ResponseWriter, r *http.Request) {
	snap := s.debug.Load()
	if snap == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no requests yet"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snap)
}

// handleDebugContext returns the current context window as stability-ordered zones.
func (s *Server) handleDebugContext(w http.ResponseWriter, r *http.Request) {
	snap := s.debug.Load()
	if snap == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no requests yet"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snap.Context)
}

// handleProprioceptive returns the last 50 entries from the proprioceptive JSONL log
// plus a placeholder light cone status.
//
//	GET /v1/proprioceptive
//	200 → { entries, light_cone }
func (s *Server) handleProprioceptive(w http.ResponseWriter, r *http.Request) {
	logPath := filepath.Join(s.cfg.WorkspaceRoot, ".cog", "run", "proprioceptive.jsonl")

	entries := readLastJSONLEntries(logPath, 50)

	// Build light cone summary from real data when available.
	lcStatus := map[string]interface{}{
		"active":          false,
		"layers":          0,
		"layer_norms":     []float64{},
		"compressed_norm": 0.0,
	}
	if lcm := s.process.LightCones(); lcm != nil {
		count := lcm.Count()
		lcStatus["active"] = count > 0
		lcStatus["count"] = count
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"entries":    entries,
		"light_cone": lcStatus,
	})
}

// handleLedger exposes QueryLedger over HTTP. Query params map 1:1 with the
// MCP tool input.
//
//	GET /v1/ledger?session_id=…&event_type=…&after_seq=…&since_timestamp=…&limit=…&verify_chain=…
//	200 → { count, events, truncated, verification, next_after_seq? }
//	400 → malformed query (bad int, bad RFC3339, after_seq without session_id)
//	404 → session_id specified but no events.jsonl found
//	500 → read/JSON failure
//
// Returns 200 with verification.valid=false when the chain is broken but data
// read succeeded — tamper evidence is the point of the ledger, so hiding data
// behind 500 would defeat the purpose.
func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	q := r.URL.Query()
	query := LedgerQuery{
		SessionID:      q.Get("session_id"),
		EventType:      q.Get("event_type"),
		SinceTimestamp: q.Get("since_timestamp"),
	}
	if v := q.Get("after_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeLedgerError(w, http.StatusBadRequest, fmt.Sprintf("after_seq: %v", err))
			return
		}
		query.AfterSeq = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeLedgerError(w, http.StatusBadRequest, fmt.Sprintf("limit: %v", err))
			return
		}
		if n < 0 {
			writeLedgerError(w, http.StatusBadRequest, "limit must be non-negative")
			return
		}
		query.Limit = n
	}
	if v := q.Get("verify_chain"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeLedgerError(w, http.StatusBadRequest, fmt.Sprintf("verify_chain: %v", err))
			return
		}
		query.VerifyChain = b
	}

	result, err := QueryLedger(s.cfg.WorkspaceRoot, query)
	if err != nil {
		switch {
		case errors.Is(err, ErrAfterSeqRequiresSession):
			writeLedgerError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrSessionNotFound):
			writeLedgerError(w, http.StatusNotFound, err.Error())
		default:
			// Filter-parse errors from QueryLedger (e.g. bad since_timestamp)
			// are user input problems. Everything else is a 500.
			msg := err.Error()
			if strings.Contains(msg, "since_timestamp") || strings.Contains(msg, "event_type") {
				writeLedgerError(w, http.StatusBadRequest, msg)
				return
			}
			writeLedgerError(w, http.StatusInternalServerError, msg)
		}
		return
	}

	_ = json.NewEncoder(w).Encode(result)
}

// writeLedgerError writes a JSON error response with the given status code.
func writeLedgerError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleTraces exposes the unified kernel trace search surface.
//
// Per Agent Q's design (2026-04-21) this is additive — /v1/proprioceptive
// stays byte-for-byte identical because dashboard.html:1265 and
// canvas.html:1706 consume its exact {entries, light_cone} shape.
//
//	GET /v1/traces
//	    ?source=attention      (or "all", default)
//	    &level=…               (applies to sources with a level-like field)
//	    &session_id=…
//	    &substring=…           (case-insensitive, full-line match)
//	    &since=5m              (RFC3339 OR Go duration)
//	    &until=…               (RFC3339 upper bound)
//	    &limit=100             (1..1000)
//	    &order=desc            ("asc" | "desc")
//
//	200 → TraceQueryResult
//	400 → unknown source / unparseable since|until / limit out of range / substring too long
//	500 → I/O error
func (s *Server) handleTraces(w http.ResponseWriter, r *http.Request) {
	q, err := parseTraceQueryFromRequest(r)
	if err != nil {
		writeTraceError(w, http.StatusBadRequest, err)
		return
	}
	result, err := QueryTraces(s.cfg.WorkspaceRoot, q)
	if err != nil {
		writeTraceError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// parseTraceQueryFromRequest maps query params onto a TraceQuery.
// Defaults: source=all, limit=100, order=desc.
func parseTraceQueryFromRequest(r *http.Request) (TraceQuery, error) {
	q := TraceQuery{
		Source:    TraceSource(strings.TrimSpace(r.URL.Query().Get("source"))),
		Level:     strings.TrimSpace(r.URL.Query().Get("level")),
		SessionID: strings.TrimSpace(r.URL.Query().Get("session_id")),
		Substring: r.URL.Query().Get("substring"),
		Order:     strings.TrimSpace(r.URL.Query().Get("order")),
	}

	if q.Source == "" {
		q.Source = SourceAll
	}
	// Validate source upfront so unknown values surface as 400, not 500.
	if _, err := resolveSources(q.Source); err != nil {
		return TraceQuery{}, err
	}

	now := time.Now()
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := ParseTraceDurationOrTime(s, now)
		if err != nil {
			return TraceQuery{}, fmt.Errorf("since: %w", err)
		}
		q.Since = t
	}
	if s := r.URL.Query().Get("until"); s != "" {
		t, err := ParseTraceDurationOrTime(s, now)
		if err != nil {
			return TraceQuery{}, fmt.Errorf("until: %w", err)
		}
		q.Until = t
	}
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return TraceQuery{}, fmt.Errorf("limit: expected non-negative integer, got %q", s)
		}
		if n > maxTracesLimit {
			return TraceQuery{}, fmt.Errorf("limit: %d exceeds max %d", n, maxTracesLimit)
		}
		q.Limit = n
	}
	return q, nil
}

// writeTraceError emits a {"error": "..."} JSON body with the given status.
// Matches the existing serve.go convention (handleDebugLast etc.).
func writeTraceError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// handleLightCone returns light cone metadata from the LightConeManager.
// When TRM is loaded, returns real per-conversation light cone states.
// When TRM is not available, returns a placeholder indicating TRM is disabled.
//
//	GET /v1/lightcone
//	200 → { active, count, cones: [...] } or placeholder
func (s *Server) handleLightCone(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	lcm := s.process.LightCones()
	if lcm == nil {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"active":          false,
			"count":           0,
			"cones":           []LightConeInfo{},
			"layers":          0,
			"layer_norms":     []float64{},
			"compressed_norm": 0.0,
			"note":            "TRM not loaded. Configure trm_weights_path in kernel.yaml to enable.",
		})
		return
	}

	cones := lcm.List()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"active": len(cones) > 0,
		"count":  len(cones),
		"cones":  cones,
	})
}

// handleConversation returns the conversation turns for a session.
//
//	GET /v1/conversation
//	  ?session_id=…            default: current process session
//	  &after_turn=N            pagination: turn_index > N
//	  &before_turn=N           reverse pagination: turn_index < N
//	  &since=RFC3339           time filter
//	  &limit=20                default 20, max 200
//	  &include_full=true       default true — hydrate from sidecar
//	  &include_tools=true      default true — include tool-call transcript
//	  &order=asc               asc (default) | desc
//
//	200 → ConversationQueryResult
//	400 → parse error
//
// Backed by the turn.completed ledger event stream + the per-session
// sidecar JSONL. Closes Agent F gap #4.
func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionID := q.Get("session_id")
	if sessionID == "" && s.process != nil {
		sessionID = s.process.SessionID()
	}
	cq := ConversationQuery{
		SessionID:    sessionID,
		IncludeFull:  parseBoolDefault(q.Get("include_full"), true),
		IncludeTools: parseBoolDefault(q.Get("include_tools"), true),
		Order:        q.Get("order"),
	}
	if v := q.Get("after_turn"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeConversationError(w, http.StatusBadRequest, "invalid after_turn: "+err.Error())
			return
		}
		cq.AfterTurn = n
	}
	if v := q.Get("before_turn"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeConversationError(w, http.StatusBadRequest, "invalid before_turn: "+err.Error())
			return
		}
		cq.BeforeTurn = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeConversationError(w, http.StatusBadRequest, "invalid limit: "+err.Error())
			return
		}
		cq.Limit = n
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeConversationError(w, http.StatusBadRequest, "invalid since (want RFC3339): "+err.Error())
			return
		}
		cq.Since = t
	}

	res, err := QueryConversation(s.cfg.WorkspaceRoot, cq)
	if err != nil {
		writeConversationError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func writeConversationError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func parseBoolDefault(s string, def bool) bool {
	if s == "" {
		return def
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return def
	}
	return b
}

// readLastJSONLEntries reads the last n lines from a JSONL file and returns
// them as a slice of parsed JSON objects. If the file does not exist or is
// empty, it returns an empty slice (never nil).
func readLastJSONLEntries(path string, n int) []json.RawMessage {
	f, err := os.Open(path)
	if err != nil {
		return []json.RawMessage{}
	}
	defer f.Close()

	// Read all lines, keeping the last n. For typical proprioceptive logs
	// (hundreds to low-thousands of entries) this is efficient enough.
	var lines []string
	scanner := bufio.NewScanner(f)
	// Allow up to 1 MB per line to handle large entries.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}

	// Take the last n lines.
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	entries := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		// Validate that it's valid JSON before including it.
		raw := json.RawMessage(line)
		if json.Valid(raw) {
			entries = append(entries, raw)
		} else {
			slog.Warn("proprioceptive: skipping invalid JSON line", "path", path)
		}
	}
	return entries
}

// sanitizeErrorMessage strips URLs and long alphanumeric strings (potential API
// keys) from an error message before returning it to clients.
var (
	reURL    = regexp.MustCompile(`https?://[^\s"',]+`)
	reAPIKey = regexp.MustCompile(`\b[A-Za-z0-9_\-]{32,}\b`)
)

func sanitizeErrorMessage(msg string) string {
	msg = reURL.ReplaceAllString(msg, "[redacted-url]")
	msg = reAPIKey.ReplaceAllString(msg, "[redacted]")
	return msg
}
