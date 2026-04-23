// serve_sessions_channel.go — kernel-side HTTP forwarder for channel-session
// registration (ADR-082, Wave 2 of the mod3-kernel integration).
//
// Design locks (Wave 2 handoff):
//
//  1. Session authority = kernel-owned. The kernel mints `session_id` if the
//     caller doesn't supply one. Mod3's SessionRegistry stores per-channel
//     state (voice, queue, device) keyed on the kernel-issued ID. If mod3
//     crashes, the kernel knows which sessions existed; mod3 rebuilds on
//     re-register.
//  2. MCP transport = HTTP proxy. Kernel reaches mod3 over plain HTTP at
//     Config.Mod3URL (default http://localhost:7860), not stdio MCP.
//
// Path namespace choice: these routes live under `/v1/channel-sessions/*`,
// NOT `/v1/sessions/*`. The kernel's existing `/v1/sessions/*` family
// (serve_sessions_mgmt.go) serves agent-session state (3-component hyphen-
// validated session IDs, workspace/role required, tied to the handoff
// protocol). Channel-participant registration has an incompatible shape
// (short UUID session IDs, participant_id/participant_type/voice/device
// fields). Rather than weaken ValidateSessionID (which would cascade into
// handoff claim semantics) we namespace the new concern. The channel-provider
// RFC's guidance to "use the same cogos_session_register primitive with a
// participant_type discriminator" remains aspirational at the MCP tool layer
// (Wave 3); at the HTTP layer the two surfaces coexist cleanly.
//
// Routes owned by this file:
//
//	POST /v1/channel-sessions/register             — mint+forward to mod3
//	POST /v1/channel-sessions/{id}/deregister      — forward deregister
//	GET  /v1/channel-sessions                      — list (kernel view + mod3 list)
//	GET  /v1/channel-sessions/{id}                 — single-session detail
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ─── in-memory registry for kernel-owned channel-session identity ────────────

// ChannelSessionRecord is the kernel's identity-authority record for a
// channel-participant session. Distinct from SessionState in sessions.go
// (which tracks agent sessions with strict 3-component hyphen IDs); the
// channel-session concern uses short UUIDs and caller-supplied participant
// metadata that the kernel passes through to mod3.
type ChannelSessionRecord struct {
	// SessionID is the kernel-authoritative ID. Either caller-supplied or
	// minted by the kernel (uuid short form).
	SessionID string `json:"session_id"`

	// ParticipantID / ParticipantType / PreferredVoice / PreferredOutputDevice
	// / Priority mirror the mod3 SessionRegisterRequest shape. The kernel
	// stores them so a post-crash re-register can replay identity cleanly.
	ParticipantID         string `json:"participant_id,omitempty"`
	ParticipantType       string `json:"participant_type,omitempty"`
	PreferredVoice        string `json:"preferred_voice,omitempty"`
	PreferredOutputDevice string `json:"preferred_output_device,omitempty"`
	Priority              int    `json:"priority,omitempty"`

	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`

	// Source records whether the session_id came from the caller or was
	// minted. Useful for audit; no functional impact.
	IDSource string `json:"id_source,omitempty"` // "caller" | "minted"
}

// ChannelSessionRegistry is the in-memory map keyed by session_id. It holds
// kernel-owned identity only; per-channel state (assigned_voice, queue,
// device) lives in mod3 and is returned as part of the merged forward
// response. If mod3 crashes the kernel knows which sessions existed and
// callers can re-register to rebuild.
type ChannelSessionRegistry struct {
	mu   sync.RWMutex
	rows map[string]*ChannelSessionRecord
}

// NewChannelSessionRegistry returns an empty registry.
func NewChannelSessionRegistry() *ChannelSessionRegistry {
	return &ChannelSessionRegistry{rows: make(map[string]*ChannelSessionRecord)}
}

// Put stores (or overwrites) a record by session_id.
func (r *ChannelSessionRegistry) Put(rec ChannelSessionRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := rec
	r.rows[rec.SessionID] = &cp
}

// Get returns a copy of the record for id, or (nil, false).
func (r *ChannelSessionRegistry) Get(id string) (*ChannelSessionRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	row, ok := r.rows[id]
	if !ok {
		return nil, false
	}
	cp := *row
	return &cp, true
}

// Delete removes the record for id. No-op if absent; returns whether the row
// was present before removal (handy for logging / telemetry).
func (r *ChannelSessionRegistry) Delete(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.rows[id]
	delete(r.rows, id)
	return ok
}

// Snapshot returns a copy of every record.
func (r *ChannelSessionRegistry) Snapshot() []*ChannelSessionRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ChannelSessionRecord, 0, len(r.rows))
	for _, row := range r.rows {
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// Len returns the number of tracked channel sessions.
func (r *ChannelSessionRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.rows)
}

// ─── route registration ──────────────────────────────────────────────────────

// defaultMod3ForwardTimeout is the per-request timeout for channel-session
// HTTP forwards. 8s is deliberately shorter than WriteTimeout (300s) so a
// stalled mod3 doesn't block a kernel request the whole way through.
const defaultMod3ForwardTimeout = 8 * time.Second

// mod3HTTPClient is the shared net/http client for forwards. Tests override
// Server.mod3Client for isolation; when nil, handlers fall back to this.
var mod3HTTPClient = &http.Client{Timeout: defaultMod3ForwardTimeout}

// registerChannelSessionRoutes attaches the 4 channel-session routes onto mux.
// Called from NewServer.
func (s *Server) registerChannelSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/channel-sessions/register", s.handleChannelSessionRegister)
	mux.HandleFunc("POST /v1/channel-sessions/{id}/deregister", s.handleChannelSessionDeregister)
	mux.HandleFunc("GET /v1/channel-sessions", s.handleChannelSessionList)
	mux.HandleFunc("GET /v1/channel-sessions/{id}", s.handleChannelSessionGet)
}

// ─── wire types ──────────────────────────────────────────────────────────────

// channelSessionRegisterRequest is the kernel-facing request body. Shape
// mirrors mod3's SessionRegisterRequest (see mod3/http_api.py line ~278) plus
// an optional session_id — when omitted, the kernel mints one.
type channelSessionRegisterRequest struct {
	SessionID             string `json:"session_id,omitempty"`
	ParticipantID         string `json:"participant_id"`
	ParticipantType       string `json:"participant_type,omitempty"`
	PreferredVoice        string `json:"preferred_voice,omitempty"`
	PreferredOutputDevice string `json:"preferred_output_device,omitempty"`
	Priority              int    `json:"priority,omitempty"`
}

// channelSessionResponse is the merged shape returned from the kernel. The
// `kernel` block is the identity record (authoritative owner); `mod3` is the
// live channel state (voice pool, queue, device). Callers get everything in
// one round trip. Unknown fields from mod3 are preserved under `mod3` as a
// raw JSON object.
type channelSessionResponse struct {
	Kernel *ChannelSessionRecord `json:"kernel"`
	Mod3   json.RawMessage       `json:"mod3,omitempty"`
}

// channelSessionListResponse is the shape of GET /v1/channel-sessions. The
// kernel list is always present; the mod3 block is an opaque pass-through so
// clients don't need to know mod3's schema.
type channelSessionListResponse struct {
	Kernel []*ChannelSessionRecord `json:"kernel"`
	Mod3   json.RawMessage         `json:"mod3,omitempty"`
}

// ─── POST /v1/channel-sessions/register ──────────────────────────────────────

func (s *Server) handleChannelSessionRegister(w http.ResponseWriter, r *http.Request) {
	var req channelSessionRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "body must be JSON")
		return
	}
	if req.ParticipantID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"participant_id is required")
		return
	}

	// Mint when absent — kernel is the session-ID authority (ADR-082).
	idSource := "caller"
	if req.SessionID == "" {
		req.SessionID = mintChannelSessionID()
		idSource = "minted"
	}
	if req.ParticipantType == "" {
		req.ParticipantType = "agent" // matches mod3's default
	}
	if req.PreferredOutputDevice == "" {
		req.PreferredOutputDevice = "system-default"
	}

	now := time.Now().UTC()
	record := ChannelSessionRecord{
		SessionID:             req.SessionID,
		ParticipantID:         req.ParticipantID,
		ParticipantType:       req.ParticipantType,
		PreferredVoice:        req.PreferredVoice,
		PreferredOutputDevice: req.PreferredOutputDevice,
		Priority:              req.Priority,
		RegisteredAt:          now,
		LastSeen:              now,
		IDSource:              idSource,
	}

	// Forward to mod3 with the kernel-issued session_id. Mod3's body is the
	// same shape modulo the optional session_id field (mod3 requires it; we
	// always supply one).
	forwardBody := map[string]any{
		"session_id":              req.SessionID,
		"participant_id":          req.ParticipantID,
		"participant_type":        req.ParticipantType,
		"preferred_voice":         req.PreferredVoice,
		"preferred_output_device": req.PreferredOutputDevice,
		"priority":                req.Priority,
	}
	body, _ := json.Marshal(forwardBody)

	mod3Resp, status, err := s.forwardMod3(r.Context(), http.MethodPost,
		"/v1/sessions/register", bytes.NewReader(body))
	if err != nil {
		slog.Warn("channel-sessions: forward to mod3 failed",
			"session_id", req.SessionID, "err", err)
		writeJSONError(w, http.StatusBadGateway, "mod3_unreachable",
			fmt.Sprintf("mod3 unreachable: %v", err))
		return
	}

	// Preserve non-success status bodies so the caller can surface mod3's
	// diagnostic text. Any 4xx/5xx from mod3 becomes the response status
	// with the raw body attached — the kernel does NOT write its identity
	// record in that case (mod3 rejected the registration).
	if status < 200 || status >= 300 {
		slog.Warn("channel-sessions: mod3 returned non-2xx",
			"session_id", req.SessionID, "status", status)
		writeJSONPassThrough(w, status, mod3Resp)
		return
	}

	// Mod3 accepted — commit the kernel-side identity record.
	s.channelSessionRegistry.Put(record)
	slog.Info("channel-sessions: registered",
		"session_id", req.SessionID, "participant_id", req.ParticipantID,
		"id_source", idSource)

	// Return merged response. The `mod3` field is the raw body from mod3 so
	// callers get the exact {assigned_voice, voice_conflict, output_device,
	// queue_depth, ...} shape mod3 emits.
	resp := channelSessionResponse{
		Kernel: &record,
		Mod3:   mod3Resp,
	}
	writeJSONResp(w, http.StatusOK, resp)
}

// ─── POST /v1/channel-sessions/{id}/deregister ───────────────────────────────

func (s *Server) handleChannelSessionDeregister(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"session_id required in path")
		return
	}

	mod3Resp, status, err := s.forwardMod3(r.Context(), http.MethodPost,
		"/v1/sessions/"+id+"/deregister", nil)
	if err != nil {
		slog.Warn("channel-sessions: deregister forward failed",
			"session_id", id, "err", err)
		writeJSONError(w, http.StatusBadGateway, "mod3_unreachable",
			fmt.Sprintf("mod3 unreachable: %v", err))
		return
	}

	// Kernel drops its identity record whenever mod3 successfully
	// acknowledges, including 404 ("never registered" is equivalent to
	// "not tracked"; clean slate in kernel matches clean slate in mod3).
	if status >= 200 && status < 500 {
		s.channelSessionRegistry.Delete(id)
	}

	writeJSONPassThrough(w, status, mod3Resp)
}

// ─── GET /v1/channel-sessions ────────────────────────────────────────────────

func (s *Server) handleChannelSessionList(w http.ResponseWriter, r *http.Request) {
	mod3Resp, status, err := s.forwardMod3(r.Context(), http.MethodGet,
		"/v1/sessions", nil)
	if err != nil {
		slog.Warn("channel-sessions: list forward failed", "err", err)
		writeJSONError(w, http.StatusBadGateway, "mod3_unreachable",
			fmt.Sprintf("mod3 unreachable: %v", err))
		return
	}
	if status < 200 || status >= 300 {
		writeJSONPassThrough(w, status, mod3Resp)
		return
	}
	resp := channelSessionListResponse{
		Kernel: s.channelSessionRegistry.Snapshot(),
		Mod3:   mod3Resp,
	}
	writeJSONResp(w, http.StatusOK, resp)
}

// ─── GET /v1/channel-sessions/{id} ───────────────────────────────────────────

func (s *Server) handleChannelSessionGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"session_id required in path")
		return
	}

	mod3Resp, status, err := s.forwardMod3(r.Context(), http.MethodGet,
		"/v1/sessions/"+id, nil)
	if err != nil {
		slog.Warn("channel-sessions: get forward failed",
			"session_id", id, "err", err)
		writeJSONError(w, http.StatusBadGateway, "mod3_unreachable",
			fmt.Sprintf("mod3 unreachable: %v", err))
		return
	}
	if status < 200 || status >= 300 {
		writeJSONPassThrough(w, status, mod3Resp)
		return
	}

	kernelRec, _ := s.channelSessionRegistry.Get(id)
	resp := channelSessionResponse{
		Kernel: kernelRec,
		Mod3:   mod3Resp,
	}
	writeJSONResp(w, http.StatusOK, resp)
}

// ─── forwarder + helpers ─────────────────────────────────────────────────────

// forwardMod3 is the single HTTP egress point to mod3. Returns the raw
// response body, HTTP status, and a transport error (non-nil only when the
// request never got a status back — connection refused, DNS, TLS, timeout).
// Mod3 error bodies (4xx/5xx) come back with err == nil and caller decides
// how to surface them.
func (s *Server) forwardMod3(ctx context.Context, method, path string, body io.Reader) (json.RawMessage, int, error) {
	base := strings.TrimRight(s.cfg.Mod3URL, "/")
	if base == "" {
		return nil, 0, errors.New("Mod3URL not configured")
	}
	url := base + path

	// Scope a per-request timeout on top of the shared client's. The ambient
	// request context may have a longer deadline; we want the forward to
	// fail fast if mod3 stalls.
	reqCtx, cancel := context.WithTimeout(ctx, defaultMod3ForwardTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, url, body)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	client := s.mod3Client
	if client == nil {
		client = mod3HTTPClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}
	if len(raw) == 0 {
		// Mod3's deregister can return a JSON object; other endpoints
		// always return JSON; if the body is empty we substitute null so
		// json.RawMessage doesn't marshal an empty string (invalid JSON).
		raw = []byte("null")
	}
	if !json.Valid(raw) {
		// Surface as a synthetic JSON string; don't propagate raw bytes.
		wrapped, _ := json.Marshal(map[string]string{"raw": string(raw)})
		return json.RawMessage(wrapped), resp.StatusCode, nil
	}
	return json.RawMessage(raw), resp.StatusCode, nil
}

// mintChannelSessionID returns a 12-char lowercase-hex short UUID. Short
// enough to be human-readable in logs, unique enough to avoid collisions
// across a single mod3 instance's lifetime.
func mintChannelSessionID() string {
	u := uuid.New()
	hex := strings.ReplaceAll(u.String(), "-", "")
	return "cs-" + hex[:12]
}

// writeJSONPassThrough writes the given status + raw JSON body verbatim. Used
// when mod3's response (especially errors) should flow to the caller intact.
func writeJSONPassThrough(w http.ResponseWriter, status int, body json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(body) == 0 {
		_, _ = w.Write([]byte("null"))
		return
	}
	_, _ = w.Write(body)
}
