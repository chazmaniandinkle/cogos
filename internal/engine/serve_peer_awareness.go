// serve_peer_awareness.go — HTTP surface for the peer-awareness packet.
//
// Phase 1B of the 4E ambient-awareness loop. The single endpoint returns
// a pre-rendered, token-budgeted string that a UserPromptSubmit hook can
// prepend to its next prompt without running any of the composition logic
// client-side. The hook's contract matches rfc003's pre-staged encoder.
//
// Route:
//
//	GET /v1/peer-awareness?sid=<sid>&budget=<N>&window=<duration>
//	                     &include_peers=<bool>
//
// Response shape is locked by the contract — packet (string), token_count
// (int), sources ([]{sid,type,ts,ref,echo_risk,...}). See
// peer_awareness_query.go for the composition details; this file handles
// only input parsing, status-code policy, and the final JSON marshal.
//
// Status codes:
//   200 — packet rendered (possibly empty when no data fits).
//   400 — sid missing or fails ValidateSid; malformed budget/window.
//   500 — unexpected render error (shouldn't happen; composer returns a
//         partial packet on section failures).
//   503 — reserved for dependency outage. Today never triggered because
//         the composer degrades each section independently; kept as a
//         policy slot so a future dependency-monitor can promote the
//         status from a best-effort 200-empty to an explicit 503.

package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// registerPeerAwarenessRoutes attaches the one route this file owns onto
// mux. Called from NewServer alongside the other registerXxxRoutes helpers.
func (s *Server) registerPeerAwarenessRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/peer-awareness", s.handlePeerAwareness)
}

// handlePeerAwareness dispatches to RenderPeerAwarenessPacket and marshals
// the PeerAwarenessResult as JSON. Defaults match the spec — budget=500,
// window=15m, include_peers=true. Unknown query params are ignored (same
// policy as the rest of /v1/bus/*).
func (s *Server) handlePeerAwareness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	req, err := parsePeerAwarenessQuery(r)
	if err != nil {
		writePeerAwarenessError(w, http.StatusBadRequest, err.Error())
		return
	}

	deps := NewPeerAwarenessDepsFromServer(s)
	result, err := RenderPeerAwarenessPacket(deps, req)
	if err != nil {
		// ValidateSid is the only documented error today; surface as 400
		// because any other composer failure is internal.
		if strings.Contains(err.Error(), "sid") {
			writePeerAwarenessError(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Warn("peer_awareness: render failed", "sid", req.Sid, "err", err)
		writePeerAwarenessError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if result == nil {
		// Defensive — the composer always returns a non-nil result today.
		writePeerAwarenessError(w, http.StatusInternalServerError, "nil result")
		return
	}

	_ = json.NewEncoder(w).Encode(result)
}

// parsePeerAwarenessQuery pulls sid/budget/window/include_peers out of r
// and normalizes them into a PeerAwarenessRequest. Validation failures
// yield a descriptive error so the caller gets a useful 400 body.
func parsePeerAwarenessQuery(r *http.Request) (PeerAwarenessRequest, error) {
	q := r.URL.Query()
	sid := strings.TrimSpace(q.Get("sid"))
	if sid == "" {
		return PeerAwarenessRequest{}, fmt.Errorf("sid query parameter is required")
	}
	if err := ValidateSid(sid); err != nil {
		return PeerAwarenessRequest{}, fmt.Errorf("invalid sid: %w", err)
	}

	req := PeerAwarenessRequest{
		Sid:          sid,
		IncludePeers: true, // default
	}

	if v := q.Get("budget"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return PeerAwarenessRequest{}, fmt.Errorf("budget must be an integer: %w", err)
		}
		if n < 0 {
			return PeerAwarenessRequest{}, fmt.Errorf("budget must be >= 0 (got %d)", n)
		}
		req.Budget = n
	}
	if v := q.Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return PeerAwarenessRequest{}, fmt.Errorf("window must be a duration like '15m': %w", err)
		}
		if d <= 0 {
			return PeerAwarenessRequest{}, fmt.Errorf("window must be > 0 (got %s)", d)
		}
		req.Window = d
	}
	if v := q.Get("include_peers"); v != "" {
		switch strings.ToLower(v) {
		case "true", "1", "yes", "y":
			req.IncludePeers = true
		case "false", "0", "no", "n":
			req.IncludePeers = false
		default:
			return PeerAwarenessRequest{}, fmt.Errorf("include_peers must be true or false (got %q)", v)
		}
	}
	return req, nil
}

// writePeerAwarenessError renders a simple {error, status} body. Matches
// the convention used by serve_bus.go + serve_sessions_mgmt.go so client
// decoders don't need a special-case path for this endpoint.
func writePeerAwarenessError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error":  msg,
		"status": status,
	})
}
