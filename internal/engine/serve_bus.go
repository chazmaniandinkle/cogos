// serve_bus.go — HTTP surface for the per-bus event store.
//
// Track 5 Phase 3: ported verbatim from the root package's bus_api.go +
// serve_bus.go. The response shapes are byte-compat with the live v3 daemon
// so cog-sandbox-mcp's bridge (tools/cogos_bridge.py) keeps working when the
// installed binary flips to engine in Phase 4.
//
// Routes owned by this file:
//
//	POST   /v1/bus/send                       — append event to a bus
//	POST   /v1/bus/open                       — create/register a bus
//	GET    /v1/bus/list                       — list all registered buses
//	GET    /v1/bus/events                     — cross-bus event search
//	GET    /v1/bus/{bus_id}/events            — per-bus events, filtered
//	GET    /v1/bus/{bus_id}/events/{seq}      — single event by seq
//	GET    /v1/bus/{bus_id}/stats             — bus statistics
//	GET    /v1/bus/consumers                  — list consumer cursors (ADR-061)
//	DELETE /v1/bus/consumers/{consumer_id}    — remove a consumer cursor
package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/cogos-dev/cogos/pkg/cogfield"
)

// registerBusRoutes attaches the nine /v1/bus/* routes + the two
// /v1/sessions routes onto mux. Called from NewServer after the base
// routes so the specific patterns register cleanly.
//
// Route ordering note: the Go 1.22+ mux routes by pattern specificity, so
// POST /v1/bus/send wins over POST /v1/bus/... even though the latter is a
// shorter prefix. We still list the specific routes before the bus_id catch
// for readability.
func (s *Server) registerBusRoutes(mux *http.ServeMux) {
	// Specific first, catch-all later — same order as root's serve.go.
	mux.HandleFunc("POST /v1/bus/send", s.handleBusSend)
	mux.HandleFunc("POST /v1/bus/open", s.handleBusOpen)
	mux.HandleFunc("GET /v1/bus/list", s.handleBusList)
	mux.HandleFunc("GET /v1/bus/events", s.handleBusEventsGlobal) // cross-bus search

	// Consumer cursor API (ADR-061).
	mux.HandleFunc("GET /v1/bus/consumers", s.handleBusConsumers)
	mux.HandleFunc("DELETE /v1/bus/consumers/", s.handleBusConsumerDelete)

	// Catch-all for /v1/bus/{bus_id}/events[,/{seq}] and /v1/bus/{bus_id}/stats.
	mux.HandleFunc("GET /v1/bus/", s.handleBusRoute)

	// Session surface.
	mux.HandleFunc("GET /v1/sessions", s.handleListSessions)
	mux.HandleFunc("GET /v1/sessions/", s.handleSessionContext)
}

// ─── POST /v1/bus/send ───────────────────────────────────────────────────────

// busSendRequest mirrors root's body shape exactly.
type busSendRequest struct {
	BusID   string `json:"bus_id"`
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"` // event type, defaults to "message"
}

// busSendResponse is the 200 body.
type busSendResponse struct {
	OK   bool   `json:"ok"`
	Seq  int    `json:"seq"`
	Hash string `json:"hash"`
}

// handleBusSend appends a message event to a bus. The engine treats "message"
// as the default event type and builds the payload identically to root:
// {"content": message} with an optional "to" field.
func (s *Server) handleBusSend(w http.ResponseWriter, r *http.Request) {
	var req busSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if req.BusID == "" || req.Message == "" {
		http.Error(w, `{"error":"bus_id and message are required"}`, http.StatusBadRequest)
		return
	}

	if req.From == "" {
		req.From = "anonymous"
	}
	eventType := req.Type
	if eventType == "" {
		eventType = "message"
	}

	mgr := s.busSessions
	if mgr == nil {
		http.Error(w, `{"error":"no bus manager available"}`, http.StatusServiceUnavailable)
		return
	}

	if err := mgr.EnsureBus(req.BusID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	payload := map[string]interface{}{
		"content": req.Message,
	}
	if req.To != "" {
		payload["to"] = req.To
	}

	evt, err := mgr.AppendEvent(req.BusID, eventType, req.From, payload)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"append failed: %s"}`, err), http.StatusInternalServerError)
		return
	}

	// Publish to the SSE broker so subscribers hear about it. Root wires
	// publish via an AppendEvent handler registered in serve_daemon.go;
	// here we do it inline so the broker is optional — if no broker is
	// attached, sends still succeed.
	if s.busBroker != nil {
		s.busBroker.Publish(req.BusID, evt)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(busSendResponse{
		OK:   true,
		Seq:  evt.Seq,
		Hash: evt.Hash,
	})
}

// ─── POST /v1/bus/open ───────────────────────────────────────────────────────

// handleBusOpen creates or re-opens a bus for inter-workspace communication.
// Response shape preserved: {ok, bus_id, state}.
func (s *Server) handleBusOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BusID        string   `json:"bus_id"`
		Participants []string `json:"participants"`
		Transport    string   `json:"transport"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if req.BusID == "" {
		http.Error(w, `{"error":"bus_id is required"}`, http.StatusBadRequest)
		return
	}

	mgr := s.busSessions
	if mgr == nil {
		http.Error(w, `{"error":"no bus manager available"}`, http.StatusServiceUnavailable)
		return
	}

	if err := mgr.EnsureBus(req.BusID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	origin := "api"
	if len(req.Participants) > 0 {
		origin = req.Participants[0]
	}
	if err := mgr.RegisterBus(req.BusID, origin, origin); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"register failed: %s"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":     true,
		"bus_id": req.BusID,
		"state":  "active",
	})
}

// ─── GET /v1/bus/list ────────────────────────────────────────────────────────

// handleBusList returns every registered bus as a JSON array of BusRegistryEntry.
func (s *Server) handleBusList(w http.ResponseWriter, r *http.Request) {
	mgr := s.busSessions
	if mgr == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
		return
	}

	entries := mgr.LoadRegistry()
	if entries == nil {
		entries = []BusRegistryEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

// ─── Event query filtering (shared) ──────────────────────────────────────────

// busEventQueryParams holds parsed query parameters for event filtering.
type busEventQueryParams struct {
	Type   string
	From   string
	After  int
	Before int
	Limit  int
	Since  string
	Until  string
}

// parseBusEventQuery extracts query params from the request URL.
// Default limit: 100, capped at 1000.
func parseBusEventQuery(r *http.Request) busEventQueryParams {
	q := r.URL.Query()
	p := busEventQueryParams{
		Type:  q.Get("type"),
		From:  q.Get("from"),
		Since: q.Get("since"),
		Until: q.Get("until"),
		Limit: 100,
	}
	// Root reads `after` but the bridge's existing query param is also `after`;
	// honor `from_sender` as a documented alias for readability, though root
	// didn't define it — we only honor the params root actually parses.
	if v := q.Get("after"); v != "" {
		p.After, _ = strconv.Atoi(v)
	}
	// after_seq is the cogos-bridge's canonical name for per-bus pagination.
	// Root accepts `after` only, but the bridge sends `after_seq`. Accept both
	// for compat — `after_seq` wins if both present.
	if v := q.Get("after_seq"); v != "" {
		n, _ := strconv.Atoi(v)
		p.After = n
	}
	if v := q.Get("before"); v != "" {
		p.Before, _ = strconv.Atoi(v)
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.Limit = n
		}
	}
	// from_sender alias (root uses `from` only, but bridge may use the
	// longer form; honor both).
	if v := q.Get("from_sender"); v != "" && p.From == "" {
		p.From = v
	}
	if p.Limit > 1000 {
		p.Limit = 1000
	}
	return p
}

// filterBusEvents applies query params to an event list.
func filterBusEvents(events []BusBlock, p busEventQueryParams) []BusBlock {
	result := make([]BusBlock, 0, len(events))
	for _, e := range events {
		if p.Type != "" && e.Type != p.Type {
			continue
		}
		if p.From != "" && e.From != p.From {
			continue
		}
		if p.After > 0 && e.Seq <= p.After {
			continue
		}
		if p.Before > 0 && e.Seq >= p.Before {
			continue
		}
		if p.Since != "" && e.Ts < p.Since {
			continue
		}
		if p.Until != "" && e.Ts > p.Until {
			continue
		}
		result = append(result, e)
		if len(result) >= p.Limit {
			break
		}
	}
	return result
}

// ─── GET /v1/bus/{bus_id}/... catch-all ──────────────────────────────────────

// handleBusRoute dispatches GET /v1/bus/{bus_id}/events, .../events/{seq},
// and .../stats.
func (s *Server) handleBusRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/bus/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, `{"error":"expected /v1/bus/{bus_id}/{action}"}`, http.StatusBadRequest)
		return
	}
	busID := parts[0]
	action := ""
	extra := ""
	if len(parts) >= 2 {
		action = parts[1]
	}
	if len(parts) >= 3 {
		extra = parts[2]
	}

	switch action {
	case "events":
		if extra != "" {
			s.handleBusEventBySeq(w, r, busID, extra)
		} else {
			s.handleBusEvents(w, r, busID)
		}
	case "stats":
		s.handleBusStats(w, r, busID)
	default:
		http.Error(w, `{"error":"expected /v1/bus/{bus_id}/events or /v1/bus/{bus_id}/stats"}`, http.StatusBadRequest)
	}
}

// handleBusEvents serves GET /v1/bus/{bus_id}/events with query params.
func (s *Server) handleBusEvents(w http.ResponseWriter, r *http.Request, busID string) {
	mgr := s.busSessions
	w.Header().Set("Content-Type", "application/json")
	if mgr == nil {
		_, _ = w.Write([]byte("[]"))
		return
	}

	events, err := mgr.ReadEvents(busID)
	if err != nil {
		_, _ = w.Write([]byte("[]"))
		return
	}

	params := parseBusEventQuery(r)
	filtered := filterBusEvents(events, params)
	// Match root: return "[]" (not null) when empty.
	if filtered == nil {
		filtered = []BusBlock{}
	}
	_ = json.NewEncoder(w).Encode(filtered)
}

// handleBusEventBySeq serves GET /v1/bus/{bus_id}/events/{seq}.
func (s *Server) handleBusEventBySeq(w http.ResponseWriter, r *http.Request, busID, seqStr string) {
	seq, err := strconv.Atoi(seqStr)
	if err != nil {
		http.Error(w, `{"error":"seq must be an integer"}`, http.StatusBadRequest)
		return
	}
	mgr := s.busSessions
	if mgr == nil {
		http.Error(w, `{"error":"event not found"}`, http.StatusNotFound)
		return
	}

	events, err := mgr.ReadEvents(busID)
	if err != nil {
		http.Error(w, `{"error":"event not found"}`, http.StatusNotFound)
		return
	}

	for _, e := range events {
		if e.Seq == seq {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(e)
			return
		}
	}

	http.Error(w, `{"error":"event not found"}`, http.StatusNotFound)
}

// ─── GET /v1/bus/{bus_id}/stats ──────────────────────────────────────────────

// busStatsResponse is the response shape for /stats. Field set + order
// matches root exactly (bus_api.go:385).
type busStatsResponse struct {
	BusID        string         `json:"bus_id"`
	EventCount   int            `json:"event_count"`
	FirstEventAt string         `json:"first_event_at,omitempty"`
	LastEventAt  string         `json:"last_event_at,omitempty"`
	Types        map[string]int `json:"types"`
	Senders      map[string]int `json:"senders"`
}

// handleBusStats serves GET /v1/bus/{bus_id}/stats.
func (s *Server) handleBusStats(w http.ResponseWriter, r *http.Request, busID string) {
	mgr := s.busSessions
	w.Header().Set("Content-Type", "application/json")
	if mgr == nil {
		_ = json.NewEncoder(w).Encode(busStatsResponse{
			BusID: busID, Types: map[string]int{}, Senders: map[string]int{},
		})
		return
	}

	events, _ := mgr.ReadEvents(busID)
	stats := busStatsResponse{
		BusID:      busID,
		EventCount: len(events),
		Types:      make(map[string]int),
		Senders:    make(map[string]int),
	}
	for i, e := range events {
		stats.Types[e.Type]++
		stats.Senders[e.From]++
		if i == 0 {
			stats.FirstEventAt = e.Ts
		}
		stats.LastEventAt = e.Ts
	}
	_ = json.NewEncoder(w).Encode(stats)
}

// ─── GET /v1/bus/events (cross-bus search) ───────────────────────────────────

// crossBusEvent wraps a BusBlock with its bus_id explicitly set (so the
// client always sees a bus_id even when the stored block didn't carry one).
// Go struct embedding preserves the JSON layout of BusBlock and appends
// bus_id — this matches root's crossBusEvent shape.
type crossBusEvent struct {
	BusBlock
	BusID string `json:"bus_id"`
}

// handleBusEventsGlobal serves GET /v1/bus/events — cross-bus event search.
// Events are sorted by timestamp descending, capped by params.Limit.
func (s *Server) handleBusEventsGlobal(w http.ResponseWriter, r *http.Request) {
	mgr := s.busSessions
	w.Header().Set("Content-Type", "application/json")
	if mgr == nil {
		_, _ = w.Write([]byte("[]"))
		return
	}

	params := parseBusEventQuery(r)
	entries := mgr.LoadRegistry()
	var allEvents []crossBusEvent

	for _, entry := range entries {
		events, err := mgr.ReadEvents(entry.BusID)
		if err != nil {
			continue
		}
		filtered := filterBusEvents(events, busEventQueryParams{
			Type:  params.Type,
			From:  params.From,
			Since: params.Since,
			Until: params.Until,
			Limit: params.Limit,
		})
		for _, e := range filtered {
			cb := crossBusEvent{BusBlock: e}
			if cb.BusBlock.BusID != "" {
				cb.BusID = cb.BusBlock.BusID
			} else {
				cb.BusID = entry.BusID
			}
			allEvents = append(allEvents, cb)
		}
	}

	sort.Slice(allEvents, func(i, j int) bool {
		return allEvents[i].Ts > allEvents[j].Ts
	})
	if len(allEvents) > params.Limit {
		allEvents = allEvents[:params.Limit]
	}
	if allEvents == nil {
		allEvents = []crossBusEvent{}
	}
	_ = json.NewEncoder(w).Encode(allEvents)
}

// ─── Consumer cursor endpoints (ADR-061) ─────────────────────────────────────

// handleBusConsumers serves GET /v1/bus/consumers — list all consumers.
// Response shape: {"consumers": [ConsumerCursor...]} — matches root exactly.
// Optional `bus_id` query filters the list to a single bus.
func (s *Server) handleBusConsumers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.busConsumers == nil {
		// Empty-object body, not an error — consistent with root's behaviour
		// when the registry hasn't been initialized.
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"consumers": []any{}})
		return
	}

	busID := r.URL.Query().Get("bus_id")
	cursors := s.busConsumers.List(busID)
	if cursors == nil {
		cursors = []*ConsumerCursor{}
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"consumers": cursors,
	})
}

// handleBusConsumerDelete serves DELETE /v1/bus/consumers/{consumer_id}.
// 204 on success, 404 if the consumer didn't exist.
func (s *Server) handleBusConsumerDelete(w http.ResponseWriter, r *http.Request) {
	consumerID := strings.TrimPrefix(r.URL.Path, "/v1/bus/consumers/")
	// Strip any trailing slash or additional path segments.
	if idx := strings.Index(consumerID, "/"); idx >= 0 {
		consumerID = consumerID[:idx]
	}
	if consumerID == "" {
		http.Error(w, "Consumer ID required", http.StatusBadRequest)
		return
	}

	if s.busConsumers == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "consumer registry not ready"})
		return
	}

	if !s.busConsumers.Remove(consumerID) {
		http.Error(w, "Consumer not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Ensure cogfield import is used (compiler sanity check — the import is live
// via BusBlock/BusRegistryEntry aliases in bus_session.go, but re-import here
// keeps godoc cross-references healthy).
var _ = cogfield.Block{}
