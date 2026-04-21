package main

// serve_bus.go — SDK routes (cog:// resolution, mutation, watch) and consumer cursor handlers (ADR-061)

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	sdk "github.com/cogos-dev/cogos/sdk"
	"github.com/cogos-dev/cogos/sdk/httputil"
)

// === SDK ROUTES ===
// These handlers delegate to the SDK kernel for universal cog:// access.

// handleResolve handles GET /resolve?uri=cog://...
func (s *serveServer) handleResolve(w http.ResponseWriter, r *http.Request) {
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	uri := r.URL.Query().Get("uri")
	if uri == "" {
		s.writeError(w, http.StatusBadRequest, "missing 'uri' query parameter", "invalid_request")
		return
	}

	resource, err := kernel.ResolveContext(r.Context(), uri)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "invalid") {
			status = http.StatusBadRequest
		}
		s.writeError(w, status, err.Error(), "resolve_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resource)
}

// handleMutate handles POST /mutate
func (s *serveServer) handleMutate(w http.ResponseWriter, r *http.Request) {
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
	var req httputil.MutateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request")
		return
	}

	if req.URI == "" {
		s.writeError(w, http.StatusBadRequest, "missing 'uri' field", "invalid_request")
		return
	}

	// Convert to SDK mutation
	var mutation *sdk.Mutation
	content := []byte(req.Content)
	switch sdk.MutationOp(req.Op) {
	case sdk.MutationSet:
		mutation = sdk.NewSetMutation(content)
	case sdk.MutationPatch:
		mutation = sdk.NewPatchMutation(content)
	case sdk.MutationAppend:
		mutation = sdk.NewAppendMutation(content)
	case sdk.MutationDelete:
		mutation = sdk.NewDeleteMutation()
	default:
		s.writeError(w, http.StatusBadRequest, "invalid 'op' field: "+req.Op, "invalid_request")
		return
	}

	if req.Metadata != nil {
		for k, v := range req.Metadata {
			mutation.WithMetadata(k, v)
		}
	}

	if err := kernel.MutateContext(r.Context(), req.URI, mutation); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "read-only") {
			status = http.StatusMethodNotAllowed
		}
		s.writeError(w, status, err.Error(), "mutate_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"uri":     req.URI,
	})
}

// handleWatch handles GET /ws/watch?uri=cog://... (WebSocket)
func (s *serveServer) handleWatch(w http.ResponseWriter, r *http.Request) {
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	uri := r.URL.Query().Get("uri")
	if uri == "" {
		s.writeError(w, http.StatusBadRequest, "missing 'uri' query parameter", "invalid_request")
		return
	}

	// Upgrade to WebSocket. Origin patterns follow the server's bind address:
	// loopback stays tight, non-loopback relaxes to "*" (security is enforced
	// at the network boundary when opting into 0.0.0.0).
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatternsForBind(s.bindAddr),
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to upgrade: "+err.Error(), "server_error")
		return
	}
	defer c.CloseNow()

	// Create watcher
	ctx := r.Context()
	watcher, err := kernel.WatchURI(ctx, uri)
	if err != nil {
		c.Close(websocket.StatusInternalError, err.Error())
		return
	}
	defer watcher.Close()

	// Send initial message
	wsjson.Write(ctx, c, map[string]any{
		"type":    "connected",
		"uri":     uri,
		"message": "watching for changes",
	})

	// Forward events to WebSocket
	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusNormalClosure, "context cancelled")
			return
		case event, ok := <-watcher.Events:
			if !ok {
				c.Close(websocket.StatusNormalClosure, "watcher closed")
				return
			}

			msg := map[string]any{
				"type":      "event",
				"uri":       event.URI,
				"eventType": event.Type,
				"timestamp": event.Timestamp,
			}
			if event.Resource != nil {
				msg["resource"] = event.Resource
			}

			if err := wsjson.Write(ctx, c, msg); err != nil {
				return
			}
		}
	}
}

// handleState returns full workspace state via SDK
func (s *serveServer) handleState(w http.ResponseWriter, r *http.Request) {
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	// Resolve cog://status for full workspace state
	resource, err := kernel.Resolve("cog://status")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "resolve_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resource)
}

// handleSignals returns signal field via SDK
func (s *serveServer) handleSignals(w http.ResponseWriter, r *http.Request) {
	// Use per-request workspace kernel, fall back to default
	kernel := s.kernel
	if ws := workspaceFromRequest(r); ws != nil {
		kernel = ws.kernel
	}
	if kernel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "SDK not initialized", "server_error")
		return
	}

	// Resolve cog://signals for signal field
	resource, err := kernel.Resolve("cog://signals")
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, err.Error(), "resolve_error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resource)
}

// === Consumer Cursor Handlers (ADR-061) ===

// handleBusAck handles POST /v1/bus/{bus_id}/ack — acknowledge an event.
func (s *serveServer) handleBusAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract bus_id from path: /v1/bus/{bus_id}/ack
	path := strings.TrimPrefix(r.URL.Path, "/v1/bus/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || parts[1] != "ack" || parts[0] == "" {
		http.Error(w, "Expected /v1/bus/{bus_id}/ack", http.StatusBadRequest)
		return
	}
	busID := parts[0]

	var req struct {
		ConsumerID string `json:"consumer_id"`
		Seq        int64  `json:"seq"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.ConsumerID == "" || req.Seq <= 0 {
		http.Error(w, "consumer_id and seq (>0) are required", http.StatusBadRequest)
		return
	}

	// Guard: consumerReg initialized asynchronously in serve_daemon.go
	if s.consumerReg == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "consumer registry not ready"})
		return
	}

	cursor, err := s.consumerReg.ack(busID, req.ConsumerID, req.Seq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(cursor)
}

// handleBusConsumers handles GET /v1/bus/consumers — list all consumers.
func (s *serveServer) handleBusConsumers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Guard: consumerReg initialized asynchronously in serve_daemon.go
	if s.consumerReg == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}

	busID := r.URL.Query().Get("bus_id") // optional filter
	cursors := s.consumerReg.list(busID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"consumers": cursors,
	})
}

// handleBusConsumerDelete handles DELETE /v1/bus/consumers/{consumer_id} — remove a consumer.
func (s *serveServer) handleBusConsumerDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract consumer_id from path: /v1/bus/consumers/{consumer_id}
	consumerID := strings.TrimPrefix(r.URL.Path, "/v1/bus/consumers/")
	if consumerID == "" {
		http.Error(w, "Consumer ID required", http.StatusBadRequest)
		return
	}

	// Guard: consumerReg initialized asynchronously in serve_daemon.go
	if s.consumerReg == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "consumer registry not ready"})
		return
	}

	if !s.consumerReg.remove(consumerID) {
		http.Error(w, "Consumer not found", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
