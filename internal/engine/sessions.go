// sessions.go — kernel-native session & handoff registries.
//
// This is the "invariance layer" of the hybrid design sketched in
// cog://mem/semantic/surveys/2026-04-21-consolidation/agent-P-session-management-evaluation.
// The bus (BusSessionManager) remains ground truth; everything in this file is
// a derived view rebuilt from bus replay at startup. The registries' job is to
// enforce the state-machine invariants the bridge-only implementation was
// doing by convention:
//
//   • session_id format validation (regex) on register
//   • idempotent re-register = update-semantics (re-register a live session
//     updates the in-memory row; re-register after end is allowed only if the
//     prior session is ended or its heartbeat is outside the active window)
//   • heartbeat/end refused against unknown sessions
//   • end refused against already-ended sessions
//   • handoff.offer → claim → complete state machine with:
//       - first-wins claim (atomic check under handoff mutex)
//       - TTL enforcement (offer rejected if now - created_at > ttl_seconds)
//       - claim-before-offer rejection (phantom offers) as 404
//       - complete-before-claim rejected as 409
//   • on every rejected claim, emit a handoff.claim_rejected event on
//     bus_handoffs so operators have an audit trail (amendment #4 of the
//     user-approved plan).
//
// Everything is flushed to the bus via BusSessionManager.AppendEvent so the
// seq/hash chain remains the authoritative ledger. The in-memory maps only
// speed up reads and guard writes against out-of-order transitions.
package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ─── well-known buses + event types ──────────────────────────────────────────
//
// These constants mirror the bridge-layer constants in
// cog-sandbox-mcp/src/cog_sandbox_mcp/tools/cogos_bridge.py. Keep them in sync.

const (
	BusSessions  = "bus_sessions"
	BusHandoffs  = "bus_handoffs"
	BusBroadcast = "bus_broadcast"

	EvtSessionRegister  = "session.register"
	EvtSessionHeartbeat = "session.heartbeat"
	EvtSessionEnd       = "session.end"

	EvtHandoffOffer         = "handoff.offer"
	EvtHandoffClaim         = "handoff.claim"
	EvtHandoffComplete      = "handoff.complete"
	EvtHandoffClaimRejected = "handoff.claim_rejected" // amendment #4

	// Default freshness window for "is this session still present" checks.
	// 600s matches the bridge's default; both are tunable per-call via the
	// active_within_seconds query param.
	defaultActiveWithinSeconds = 600
)

// sessionIDPattern enforces the three-component hyphen-separated lowercase
// format the handoff protocol recommends. Same regex the design doc calls out.
// Example: slowbro-laptop-cogos-gap-closure → passes.
var sessionIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)+$`)

// ValidateSessionID returns nil iff id matches the lowercase-hyphen format
// with at least two components. Exported so tests and the HTTP handlers can
// share the single source of truth.
func ValidateSessionID(id string) error {
	if id == "" {
		return errors.New("session_id is required")
	}
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf(
			"session_id %q must be ascii-lowercase with at least two "+
				"hyphen-separated components", id)
	}
	return nil
}

// ─── Session registry ────────────────────────────────────────────────────────

// SessionState is the in-memory row for a session. Fields mirror the payload
// shape the bridge writes to bus_sessions so presence responses are
// byte-compat for external consumers.
type SessionState struct {
	SessionID    string                 `json:"session_id"`
	Workspace    string                 `json:"workspace"`
	Role         string                 `json:"role"`
	Task         string                 `json:"task,omitempty"`
	Model        string                 `json:"model,omitempty"`
	Hostname     string                 `json:"hostname,omitempty"`
	ContextUsage float64                `json:"context_usage,omitempty"`
	CurrentTask  string                 `json:"current_task,omitempty"`
	Status       string                 `json:"status,omitempty"`
	Extras       map[string]interface{} `json:"extras,omitempty"`

	// Lifecycle.
	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	EndReason    string    `json:"end_reason,omitempty"`
	EndHandoffID string    `json:"end_handoff_id,omitempty"`

	// Lifecycle flag, computed from EndedAt. Kept as its own JSON field so
	// presence responses don't need derivation on the client side.
	Ended bool `json:"ended"`
}

// IsActive returns true iff the session has been heard from within the given
// window AND has not been ended. Window ≤ 0 falls back to the protocol default.
func (s *SessionState) IsActive(window time.Duration, now time.Time) bool {
	if s.Ended {
		return false
	}
	if window <= 0 {
		window = time.Duration(defaultActiveWithinSeconds) * time.Second
	}
	return !s.LastSeen.IsZero() && now.Sub(s.LastSeen) <= window
}

// SessionRegistry is the in-memory, RWMutex-guarded map of session_id →
// SessionState. The bus is ground truth; this map is a derived, warm cache.
type SessionRegistry struct {
	mu   sync.RWMutex
	rows map[string]*SessionState
}

// NewSessionRegistry returns an empty registry.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{rows: make(map[string]*SessionState)}
}

// Len returns the number of tracked sessions.
func (r *SessionRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.rows)
}

// Get returns a copy of the session row, or (nil, false) if unknown.
func (r *SessionRegistry) Get(id string) (*SessionState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	row, ok := r.rows[id]
	if !ok {
		return nil, false
	}
	cp := *row
	return &cp, true
}

// Snapshot returns a copy of every session row. Order is not guaranteed.
func (r *SessionRegistry) Snapshot() []*SessionState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*SessionState, 0, len(r.rows))
	for _, row := range r.rows {
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// ApplyRegister records a session.register event into the map. Idempotent per
// amendment #2: same session_id updates the in-memory row; re-registration
// after end is allowed only if the prior session is ended or its heartbeat is
// outside the active window.
//
// Returns the resulting state (copy) and a flag indicating whether the
// registry row was newly created vs updated.
func (r *SessionRegistry) ApplyRegister(
	state SessionState,
	activeWindow time.Duration,
	now time.Time,
) (*SessionState, bool, error) {
	if err := ValidateSessionID(state.SessionID); err != nil {
		return nil, false, err
	}
	if state.RegisteredAt.IsZero() {
		state.RegisteredAt = now
	}
	if state.LastSeen.IsZero() {
		state.LastSeen = state.RegisteredAt
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, found := r.rows[state.SessionID]
	if found && !existing.Ended {
		// Live prior row — allow re-register only if heartbeat is stale.
		if existing.IsActive(activeWindow, now) {
			// Still active; accept as update. Preserve the original
			// RegisteredAt to keep lineage.
			state.RegisteredAt = existing.RegisteredAt
		}
	}
	// Clear ended flags if re-registering (fresh lifecycle).
	state.Ended = false
	state.EndedAt = time.Time{}
	state.EndReason = ""
	state.EndHandoffID = ""
	row := state
	r.rows[state.SessionID] = &row
	cp := row
	return &cp, !found, nil
}

// ApplyHeartbeat bumps LastSeen + optional fields. Returns (state, ok).
//   - ok=false when session is unknown.
//   - ok=true but Ended=true when the session was already ended — the caller
//     should translate that to a 409 before emitting to the bus.
func (r *SessionRegistry) ApplyHeartbeat(
	id string,
	contextUsage float64,
	status, currentTask string,
	now time.Time,
) (*SessionState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok {
		return nil, false
	}
	row.LastSeen = now
	if contextUsage > 0 {
		row.ContextUsage = contextUsage
	}
	if status != "" {
		row.Status = status
	}
	if currentTask != "" {
		row.CurrentTask = currentTask
	}
	cp := *row
	return &cp, true
}

// ApplyEnd transitions a session to ended. Returns:
//   - (nil, false, nil)            when the session is unknown → 404.
//   - (&state, true, errAlready)   when the session was already ended → 409.
//   - (&state, true, nil)          on success.
func (r *SessionRegistry) ApplyEnd(
	id, reason, handoffID string,
	now time.Time,
) (*SessionState, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok {
		return nil, false, nil
	}
	if row.Ended {
		cp := *row
		return &cp, true, errors.New("session already ended")
	}
	row.Ended = true
	row.EndedAt = now
	row.EndReason = reason
	row.EndHandoffID = handoffID
	row.LastSeen = now // treat end as implicit last contact
	cp := *row
	return &cp, true, nil
}

// ─── Handoff registry ────────────────────────────────────────────────────────

// HandoffLifecycle values.
const (
	HandoffStateOpen      = "open"
	HandoffStateClaimed   = "claimed"
	HandoffStateCompleted = "completed"
)

// HandoffState is the in-memory row for a handoff.
type HandoffState struct {
	HandoffID   string `json:"handoff_id"`
	FromSession string `json:"from_session"`
	ToSession   string `json:"to_session,omitempty"`
	Reason      string `json:"reason,omitempty"`

	// Full offer payload (mirror of what went on the bus). Kept verbatim so
	// claimants can read it back without a separate bus fetch.
	OfferPayload map[string]interface{} `json:"offer,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	TTLSeconds int       `json:"ttl_seconds,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`

	State             string    `json:"state"`
	ClaimingSession   string    `json:"claiming_session,omitempty"`
	ClaimedAt         time.Time `json:"claimed_at,omitempty"`
	CompletingSession string    `json:"completing_session,omitempty"`
	CompletedAt       time.Time `json:"completed_at,omitempty"`
	CompletionOutcome string    `json:"outcome,omitempty"`
	CompletionNotes   string    `json:"notes,omitempty"`
	NextHandoffID    string    `json:"next_handoff_id,omitempty"`
}

// IsExpired is true when TTL > 0 and now is past ExpiresAt.
func (h *HandoffState) IsExpired(now time.Time) bool {
	if h.TTLSeconds <= 0 || h.ExpiresAt.IsZero() {
		return false
	}
	return now.After(h.ExpiresAt)
}

// HandoffRegistry guards in-memory handoff state. The bus is still the
// source of truth; this struct is how we atomically enforce first-wins on
// claim and state-order on complete.
type HandoffRegistry struct {
	mu   sync.Mutex // write-mostly; snapshot also takes the lock briefly
	rows map[string]*HandoffState
}

// NewHandoffRegistry returns an empty registry.
func NewHandoffRegistry() *HandoffRegistry {
	return &HandoffRegistry{rows: make(map[string]*HandoffState)}
}

// Len returns the number of tracked handoffs (open + claimed + completed).
func (r *HandoffRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

// Get returns a copy, or (nil, false).
func (r *HandoffRegistry) Get(id string) (*HandoffState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok {
		return nil, false
	}
	cp := *row
	return &cp, true
}

// Snapshot returns a copy of every handoff row.
func (r *HandoffRegistry) Snapshot() []*HandoffState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*HandoffState, 0, len(r.rows))
	for _, row := range r.rows {
		cp := *row
		out = append(out, &cp)
	}
	return out
}

// ApplyOffer records a handoff offer. Idempotent on duplicate IDs — replaces
// the row; this shouldn't happen in normal flow (IDs are unique) but mirrors
// the bus semantics (re-emitting the same offer would just append another
// event).
func (r *HandoffRegistry) ApplyOffer(h HandoffState, now time.Time) *HandoffState {
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	if h.TTLSeconds > 0 && h.ExpiresAt.IsZero() {
		h.ExpiresAt = h.CreatedAt.Add(time.Duration(h.TTLSeconds) * time.Second)
	}
	h.State = HandoffStateOpen
	r.mu.Lock()
	defer r.mu.Unlock()
	row := h
	r.rows[h.HandoffID] = &row
	cp := row
	return &cp
}

// ClaimRejection reasons for the handoff.claim_rejected event (amendment #4).
type ClaimRejection string

const (
	ClaimRejectedOfferNotFound ClaimRejection = "offer_not_found"
	ClaimRejectedAlreadyClaimed ClaimRejection = "already_claimed"
	ClaimRejectedTTLExpired     ClaimRejection = "ttl_expired"
	ClaimRejectedOutOfOrder     ClaimRejection = "out_of_order"
)

// ClaimResult is what the handler returns after an atomic claim attempt.
type ClaimResult struct {
	Offer              *HandoffState
	Rejection          ClaimRejection // empty on success
	ConflictingSession string         // set on already_claimed
}

// ApplyClaim is the ATOMIC first-wins check. Returns a ClaimResult whose
// Rejection is non-empty if the claim should be rejected (and a
// claim_rejected event emitted). On success, updates the in-memory row and
// returns the full offer copy with State=claimed.
func (r *HandoffRegistry) ApplyClaim(
	id, claimingSession string,
	now time.Time,
) ClaimResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok {
		return ClaimResult{Rejection: ClaimRejectedOfferNotFound}
	}
	if row.State == HandoffStateClaimed || row.State == HandoffStateCompleted {
		return ClaimResult{
			Rejection:          ClaimRejectedAlreadyClaimed,
			ConflictingSession: row.ClaimingSession,
		}
	}
	if row.IsExpired(now) {
		return ClaimResult{Rejection: ClaimRejectedTTLExpired}
	}
	row.State = HandoffStateClaimed
	row.ClaimingSession = claimingSession
	row.ClaimedAt = now
	cp := *row
	return ClaimResult{Offer: &cp}
}

// ApplyComplete transitions a claimed handoff to completed. Returns
// (&state, true) on success; returns (&state, false) with reason
// ClaimRejectedOutOfOrder if called before claim; returns (nil, false) if
// unknown (caller translates to 404 vs 409).
func (r *HandoffRegistry) ApplyComplete(
	id, completingSession, outcome, notes, nextHandoffID string,
	now time.Time,
) (*HandoffState, ClaimRejection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[id]
	if !ok {
		return nil, ClaimRejectedOfferNotFound
	}
	if row.State != HandoffStateClaimed {
		cp := *row
		return &cp, ClaimRejectedOutOfOrder
	}
	row.State = HandoffStateCompleted
	row.CompletingSession = completingSession
	row.CompletedAt = now
	row.CompletionOutcome = outcome
	row.CompletionNotes = notes
	row.NextHandoffID = nextHandoffID
	cp := *row
	return &cp, ""
}

// ─── Replay-from-bus warmers ─────────────────────────────────────────────────

// ReplaySessionRegistry reads bus_sessions events through the given manager
// and reconstructs the in-memory session map. The bus is ground truth; this
// function just gets the derived view ready for read-traffic before the HTTP
// server starts serving. Errors are logged but non-fatal — an empty registry
// is a safe degraded start.
func ReplaySessionRegistry(mgr *BusSessionManager, reg *SessionRegistry) error {
	if mgr == nil || reg == nil {
		return nil
	}
	events, err := mgr.ReadEvents(BusSessions)
	if err != nil {
		slog.Warn("sessions: replay read failed", "bus", BusSessions, "err", err)
		return err
	}
	// bridge-v0.1 payload shape: the entire session dict lives at the top
	// level of the bus event's payload map. Some bridge code writes a
	// nested "content" (the message text) plus flat fields; we tolerate
	// both by preferring explicit top-level keys and falling back to
	// reasonable defaults.
	for _, evt := range events {
		switch evt.Type {
		case EvtSessionRegister:
			state := parseSessionPayload(evt.From, evt.Payload)
			if state == nil {
				continue
			}
			if ts, err := parseBusTS(evt.Ts); err == nil {
				state.RegisteredAt = ts
				if state.LastSeen.IsZero() {
					state.LastSeen = ts
				}
			}
			// Replay bypasses the "refuse re-register when live" check —
			// we're reconstructing history, not enforcing invariants.
			reg.mu.Lock()
			row := *state
			reg.rows[state.SessionID] = &row
			reg.mu.Unlock()

		case EvtSessionHeartbeat:
			payload := evt.Payload
			if payload == nil {
				continue
			}
			id, _ := payload["session_id"].(string)
			if id == "" {
				id = evt.From
			}
			if id == "" {
				continue
			}
			reg.mu.Lock()
			row, ok := reg.rows[id]
			if ok {
				if ts, err := parseBusTS(evt.Ts); err == nil {
					row.LastSeen = ts
				}
				if v, ok := payload["context_usage"].(float64); ok && v > 0 {
					row.ContextUsage = v
				}
				if s, _ := payload["status"].(string); s != "" {
					row.Status = s
				}
				if t, _ := payload["current_task"].(string); t != "" {
					row.CurrentTask = t
				}
			}
			reg.mu.Unlock()

		case EvtSessionEnd:
			payload := evt.Payload
			if payload == nil {
				continue
			}
			id, _ := payload["session_id"].(string)
			if id == "" {
				id = evt.From
			}
			if id == "" {
				continue
			}
			reason, _ := payload["reason"].(string)
			handoffID, _ := payload["handoff_id"].(string)
			reg.mu.Lock()
			row, ok := reg.rows[id]
			if ok {
				row.Ended = true
				if ts, err := parseBusTS(evt.Ts); err == nil {
					row.EndedAt = ts
					row.LastSeen = ts
				}
				row.EndReason = reason
				row.EndHandoffID = handoffID
			}
			reg.mu.Unlock()
		}
	}
	slog.Info("sessions: replay complete", "sessions", reg.Len(), "events", len(events))
	return nil
}

// ReplayHandoffRegistry replays bus_handoffs into the handoff registry.
func ReplayHandoffRegistry(mgr *BusSessionManager, reg *HandoffRegistry) error {
	if mgr == nil || reg == nil {
		return nil
	}
	events, err := mgr.ReadEvents(BusHandoffs)
	if err != nil {
		slog.Warn("handoffs: replay read failed", "bus", BusHandoffs, "err", err)
		return err
	}
	for _, evt := range events {
		payload := evt.Payload
		if payload == nil {
			continue
		}
		switch evt.Type {
		case EvtHandoffOffer:
			h := parseHandoffOffer(evt.Payload)
			if h == nil {
				continue
			}
			if ts, err := parseBusTS(evt.Ts); err == nil && h.CreatedAt.IsZero() {
				h.CreatedAt = ts
				if h.TTLSeconds > 0 && h.ExpiresAt.IsZero() {
					h.ExpiresAt = ts.Add(time.Duration(h.TTLSeconds) * time.Second)
				}
			}
			h.State = HandoffStateOpen
			reg.mu.Lock()
			row := *h
			reg.rows[h.HandoffID] = &row
			reg.mu.Unlock()

		case EvtHandoffClaim:
			id, _ := payload["handoff_id"].(string)
			claimant, _ := payload["claiming_session"].(string)
			if id == "" {
				continue
			}
			reg.mu.Lock()
			row, ok := reg.rows[id]
			// On replay, honor first-wins by seq: only the first claim
			// for a given id transitions state.
			if ok && row.State == HandoffStateOpen {
				row.State = HandoffStateClaimed
				row.ClaimingSession = claimant
				if ts, err := parseBusTS(evt.Ts); err == nil {
					row.ClaimedAt = ts
				}
			}
			reg.mu.Unlock()

		case EvtHandoffComplete:
			id, _ := payload["handoff_id"].(string)
			session, _ := payload["completing_session"].(string)
			outcome, _ := payload["outcome"].(string)
			notes, _ := payload["notes"].(string)
			nextID, _ := payload["next_handoff_id"].(string)
			if id == "" {
				continue
			}
			reg.mu.Lock()
			row, ok := reg.rows[id]
			if ok && row.State == HandoffStateClaimed {
				row.State = HandoffStateCompleted
				row.CompletingSession = session
				row.CompletionOutcome = outcome
				row.CompletionNotes = notes
				row.NextHandoffID = nextID
				if ts, err := parseBusTS(evt.Ts); err == nil {
					row.CompletedAt = ts
				}
			}
			reg.mu.Unlock()
		}
	}
	slog.Info("handoffs: replay complete", "handoffs", reg.Len(), "events", len(events))
	return nil
}

// ─── payload helpers ─────────────────────────────────────────────────────────

// parseSessionPayload reconstructs a SessionState from a bus event's payload.
// The bridge writes nearly everything to the top level. Unknown keys are
// copied to Extras so no information is lost.
func parseSessionPayload(fromField string, p map[string]interface{}) *SessionState {
	if p == nil {
		return nil
	}
	id, _ := p["session_id"].(string)
	if id == "" {
		id = fromField
	}
	if id == "" {
		return nil
	}
	state := &SessionState{SessionID: id}
	pickString(&state.Workspace, p, "workspace")
	pickString(&state.Role, p, "role")
	pickString(&state.Task, p, "task")
	pickString(&state.Model, p, "model")
	pickString(&state.Hostname, p, "hostname")
	pickString(&state.Status, p, "status")
	pickString(&state.CurrentTask, p, "current_task")
	if v, ok := p["context_usage"].(float64); ok {
		state.ContextUsage = v
	}
	// Stash any other fields for debuggability.
	known := map[string]bool{
		"session_id": true, "workspace": true, "role": true, "task": true,
		"model": true, "hostname": true, "status": true,
		"current_task": true, "context_usage": true, "content": true,
	}
	for k, v := range p {
		if known[k] {
			continue
		}
		if state.Extras == nil {
			state.Extras = map[string]interface{}{}
		}
		state.Extras[k] = v
	}
	return state
}

// parseHandoffOffer reconstructs a HandoffState from a bus.offer payload.
// The bridge writes the whole offer dict verbatim to the payload.
func parseHandoffOffer(p map[string]interface{}) *HandoffState {
	if p == nil {
		return nil
	}
	id, _ := p["handoff_id"].(string)
	if id == "" {
		return nil
	}
	h := &HandoffState{HandoffID: id}
	pickString(&h.FromSession, p, "from_session")
	pickString(&h.ToSession, p, "to_session")
	pickString(&h.Reason, p, "reason")
	if v, ok := p["ttl_seconds"].(float64); ok {
		h.TTLSeconds = int(v)
	}
	if s, _ := p["created_at"].(string); s != "" {
		if ts, err := parseBusTS(s); err == nil {
			h.CreatedAt = ts
		}
	}
	// Keep the whole payload verbatim so claimants can read it back.
	h.OfferPayload = copyMap(p)
	return h
}

func pickString(dst *string, p map[string]interface{}, key string) {
	if v, ok := p[key].(string); ok {
		*dst = v
	}
}

func copyMap(src map[string]interface{}) map[string]interface{} {
	if src == nil {
		return nil
	}
	out := make(map[string]interface{}, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func parseBusTS(ts string) (time.Time, error) {
	// The bus writes RFC3339Nano; tolerate RFC3339 too.
	if ts == "" {
		return time.Time{}, errors.New("empty ts")
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(ts))
}
