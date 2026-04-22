// serve_sessions.go — per-session context observability endpoints.
//
// Track 5 Phase 3: ported from root's serve_context.go (handleListSessions +
// handleSessionContext). Preserves the byte-compat response shape:
//
//	GET /v1/sessions           → { count, sessions: [sessionSummary ...] }
//	GET /v1/sessions/{id}      → SessionContextState (full detail)
//	GET /v1/sessions/{id}/context → SessionContextState (alias)
//
// The store is kept as a struct field on Server so tests can instantiate
// isolated instances; root's singleton-style map is equivalent in single-
// server deployments.
package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SessionContextState captures the context state for a single session's most
// recent foveated request. Mirrors root's type (serve_context.go) exactly so
// the JSON payload is byte-compatible.
type SessionContextState struct {
	SessionID      string         `json:"session_id"`
	Profile        string         `json:"profile"`
	TurnNumber     int            `json:"turn_number"`
	IrisSize       int            `json:"iris_size"`
	IrisUsed       int            `json:"iris_used"`
	IrisPressure   float64        `json:"iris_pressure"`
	TotalTokens    int            `json:"total_tokens"`
	Blocks         []SessionBlock `json:"blocks"`
	BlockCount     int            `json:"block_count"`
	CacheHits      int            `json:"cache_hits"`
	LastRequestAt  time.Time      `json:"last_request_at"`
	CoherenceScore float64        `json:"coherence_score,omitempty"`
}

// SessionBlock is the block shape embedded inside SessionContextState.
// Matches root's ContextBlock JSON shape (serve_context.go ContextBlock).
type SessionBlock struct {
	Hash    string  `json:"hash"`
	Kind    string  `json:"kind,omitempty"`
	Tokens  int     `json:"tokens"`
	Score   float64 `json:"score,omitempty"`
	Source  string  `json:"source,omitempty"`
	Tier    string  `json:"tier,omitempty"`
	Content string  `json:"content,omitempty"`
}

// SessionContextStore is the in-memory index of session → latest context
// state. Methods are goroutine-safe.
type SessionContextStore struct {
	mu    sync.RWMutex
	store map[string]*SessionContextState
}

// NewSessionContextStore constructs an empty store.
func NewSessionContextStore() *SessionContextStore {
	return &SessionContextStore{store: make(map[string]*SessionContextState)}
}

// Record replaces the state for a session.
func (s *SessionContextStore) Record(state *SessionContextState) {
	if state == nil || state.SessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store[state.SessionID] = state
}

// Get returns the state for a session.
func (s *SessionContextStore) Get(sessionID string) (*SessionContextState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.store[sessionID]
	return st, ok
}

// Snapshot returns a copy of every session's state. Slice order is not
// guaranteed — matches root (iteration over map).
func (s *SessionContextStore) Snapshot() []*SessionContextState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*SessionContextState, 0, len(s.store))
	for _, st := range s.store {
		out = append(out, st)
	}
	return out
}

// handleListSessions returns summary metadata for all known sessions.
//
//	GET /v1/sessions
//	200 → { count, sessions: [sessionSummary] }
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	type sessionSummary struct {
		SessionID      string    `json:"session_id"`
		Profile        string    `json:"profile"`
		TurnNumber     int       `json:"turn_number"`
		IrisPressure   float64   `json:"iris_pressure"`
		TotalTokens    int       `json:"total_tokens"`
		BlockCount     int       `json:"block_count"`
		CoherenceScore float64   `json:"coherence_score,omitempty"`
		LastRequestAt  time.Time `json:"last_request_at"`
	}

	snap := s.sessions.Snapshot()
	sessions := make([]sessionSummary, 0, len(snap))
	for _, state := range snap {
		sessions = append(sessions, sessionSummary{
			SessionID:      state.SessionID,
			Profile:        state.Profile,
			TurnNumber:     state.TurnNumber,
			IrisPressure:   state.IrisPressure,
			TotalTokens:    state.TotalTokens,
			BlockCount:     state.BlockCount,
			CoherenceScore: state.CoherenceScore,
			LastRequestAt:  state.LastRequestAt,
		})
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"sessions": sessions,
		"count":    len(sessions),
	})
}

// handleSessionContext returns the full context state for a specific session.
//
//	GET /v1/sessions/{session_id}
//	GET /v1/sessions/{session_id}/context
//	200 → SessionContextState
//	400 → missing session_id
//	404 → session not found
func (s *Server) handleSessionContext(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Extract session_id from URL path: /v1/sessions/{session_id}[/context]
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	sessionID := strings.TrimSuffix(path, "/context")
	sessionID = strings.TrimSuffix(sessionID, "/")

	if sessionID == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "invalid_request",
				"message": "session_id is required in path: /v1/sessions/{session_id}",
			},
		})
		return
	}

	state, ok := s.sessions.Get(sessionID)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type": "not_found",
				"message": fmt.Sprintf(
					"No context found for session %q. Use GET /v1/sessions to list known sessions.",
					sessionID),
			},
		})
		return
	}

	_ = json.NewEncoder(w).Encode(state)
}
