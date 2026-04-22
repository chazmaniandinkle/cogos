// sessions_test.go — unit tests for the kernel-native session / handoff
// hybrid (cog://mem/semantic/surveys/2026-04-21-consolidation/
// agent-P-session-management-evaluation §"Tests to add").
//
// 14 tests total:
//
//  1. TestSessionRegister_ValidID         — happy path
//  2. TestSessionRegister_InvalidFormat   — regex rejects malformed IDs
//  3. TestSessionRegister_ReRegistration  — idempotent UPDATE semantics
//  4. TestHeartbeat_UnknownSession        — 404
//  5. TestEnd_UnknownSession              — 404
//  6. TestEnd_AlreadyEnded                — 409
//  7. TestPresence_ActiveWindow           — stale heartbeats marked inactive
//  8. TestHandoffOffer_MissingTaskFields  — 400 validation
//  9. TestHandoffClaim_Atomicity          — concurrent claims, first wins
// 10. TestHandoffClaim_TTLExpired         — expired offer rejected with 409
// 11. TestHandoffClaim_PhantomOffer       — 404
// 12. TestHandoffComplete_WithoutClaim    — 409
// 13. TestReplayOnStartup                 — registry rebuilds from bus
// 14. TestClaimRejectedEventEmitted       — amendment #4 observability

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── HTTP fixtures ───────────────────────────────────────────────────────────

// newSessionsTestServer reuses the generic events fixture and returns the
// Server struct alongside an httptest base URL so tests can poke both the
// public HTTP surface and the in-memory registries directly.
func newSessionsTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog", Port: 0}
	nucleus := &Nucleus{Name: "test"}
	proc := NewProcess(cfg, nucleus)
	srv := NewServer(cfg, nucleus, proc)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = proc.Broker().Close()
	})
	return srv, ts
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response, into any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// ─── 1. TestSessionRegister_ValidID ──────────────────────────────────────────

func TestSessionRegister_ValidID(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)

	body := map[string]any{
		"session_id": "alpha-beta-gamma",
		"workspace":  "demo",
		"role":       "author",
	}
	resp := postJSON(t, ts.URL+"/v1/sessions/register", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out sessionWriteResponse
	decodeJSON(t, resp, &out)
	if !out.OK || out.SessionID != "alpha-beta-gamma" {
		t.Errorf("bad response: %+v", out)
	}
	if out.Seq != 1 {
		t.Errorf("seq = %d, want 1", out.Seq)
	}
	if got := srv.sessionRegistry.Len(); got != 1 {
		t.Errorf("registry len = %d, want 1", got)
	}
}

// ─── 2. TestSessionRegister_InvalidFormat ────────────────────────────────────

func TestSessionRegister_InvalidFormat(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)

	cases := []string{
		"",            // empty
		"single",      // only one component
		"BAD-ID-HERE", // uppercase
		"has spaces-x-y",
		"trailing-dash-",
	}
	for _, id := range cases {
		body := map[string]any{"session_id": id, "workspace": "w", "role": "r"}
		resp := postJSON(t, ts.URL+"/v1/sessions/register", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", id, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// ─── 3. TestSessionRegister_ReRegistration ───────────────────────────────────

func TestSessionRegister_ReRegistration(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)

	body := map[string]any{
		"session_id": "same-id-twice",
		"workspace":  "demo",
		"role":       "author",
		"task":       "first",
	}
	r1 := postJSON(t, ts.URL+"/v1/sessions/register", body)
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first register = %d", r1.StatusCode)
	}

	// Second call updates the row — should not be rejected.
	body["task"] = "second"
	r2 := postJSON(t, ts.URL+"/v1/sessions/register", body)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("re-register = %d, want 200 (update)", r2.StatusCode)
	}
	var resp2 sessionWriteResponse
	decodeJSON(t, r2, &resp2)
	if resp2.Created {
		t.Errorf("second register reported created=true")
	}
	if resp2.Session == nil || resp2.Session.Task != "second" {
		t.Errorf("task not updated: %+v", resp2.Session)
	}
	if got := srv.sessionRegistry.Len(); got != 1 {
		t.Errorf("len = %d, want 1 after update", got)
	}
	// Both events should be on the bus.
	events, _ := srv.busSessions.ReadEvents(BusSessions)
	if len(events) < 2 {
		t.Errorf("bus has %d events, want >=2", len(events))
	}
}

// ─── 4. TestHeartbeat_UnknownSession ─────────────────────────────────────────

func TestHeartbeat_UnknownSession(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)

	resp := postJSON(t, ts.URL+"/v1/sessions/ghost-session-id/heartbeat",
		map[string]any{"status": "live"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ─── 5. TestEnd_UnknownSession ───────────────────────────────────────────────

func TestEnd_UnknownSession(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)

	resp := postJSON(t, ts.URL+"/v1/sessions/ghost-session-id/end",
		map[string]any{"reason": "test"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ─── 6. TestEnd_AlreadyEnded ─────────────────────────────────────────────────

func TestEnd_AlreadyEnded(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)

	body := map[string]any{"session_id": "once-and-done", "workspace": "w", "role": "r"}
	postJSON(t, ts.URL+"/v1/sessions/register", body).Body.Close()

	endURL := ts.URL + "/v1/sessions/once-and-done/end"
	r1 := postJSON(t, endURL, map[string]any{"reason": "first"})
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first end = %d", r1.StatusCode)
	}
	r2 := postJSON(t, endURL, map[string]any{"reason": "second"})
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Errorf("second end = %d, want 409", r2.StatusCode)
	}
}

// ─── 7. TestPresence_ActiveWindow ────────────────────────────────────────────

func TestPresence_ActiveWindow(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)

	// Register two sessions. Then manually backdate one's LastSeen.
	postJSON(t, ts.URL+"/v1/sessions/register", map[string]any{
		"session_id": "fresh-session-here", "workspace": "w", "role": "r",
	}).Body.Close()
	postJSON(t, ts.URL+"/v1/sessions/register", map[string]any{
		"session_id": "stale-session-here", "workspace": "w", "role": "r",
	}).Body.Close()

	// Rewind the stale session's LastSeen well past the default window.
	srv.sessionRegistry.mu.Lock()
	srv.sessionRegistry.rows["stale-session-here"].LastSeen =
		time.Now().UTC().Add(-2 * time.Hour)
	srv.sessionRegistry.mu.Unlock()

	resp, err := http.Get(ts.URL + "/v1/sessions/presence?active_within_seconds=300")
	if err != nil {
		t.Fatalf("GET presence: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Sessions []struct {
			SessionID string `json:"session_id"`
			Active    bool   `json:"active"`
		} `json:"sessions"`
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Count != 2 {
		t.Fatalf("count = %d, want 2", body.Count)
	}
	for _, s := range body.Sessions {
		switch s.SessionID {
		case "fresh-session-here":
			if !s.Active {
				t.Errorf("%s: active=false, want true", s.SessionID)
			}
		case "stale-session-here":
			if s.Active {
				t.Errorf("%s: active=true, want false", s.SessionID)
			}
		}
	}
}

// ─── 8. TestHandoffOffer_MissingTaskFields ───────────────────────────────────

func TestHandoffOffer_MissingTaskFields(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)

	// No task at all.
	r1 := postJSON(t, ts.URL+"/v1/handoffs/offer", map[string]any{
		"from_session":     "src-session-abc",
		"bootstrap_prompt": "x",
	})
	r1.Body.Close()
	if r1.StatusCode != http.StatusBadRequest {
		t.Errorf("no-task: status = %d, want 400", r1.StatusCode)
	}

	// task without next_steps.
	r2 := postJSON(t, ts.URL+"/v1/handoffs/offer", map[string]any{
		"from_session":     "src-session-abc",
		"bootstrap_prompt": "x",
		"task":             map[string]any{"title": "T", "goal": "G"},
	})
	r2.Body.Close()
	if r2.StatusCode != http.StatusBadRequest {
		t.Errorf("no next_steps: status = %d, want 400", r2.StatusCode)
	}

	// Empty next_steps list.
	r3 := postJSON(t, ts.URL+"/v1/handoffs/offer", map[string]any{
		"from_session":     "src-session-abc",
		"bootstrap_prompt": "x",
		"task":             map[string]any{"title": "T", "goal": "G", "next_steps": []any{}},
	})
	r3.Body.Close()
	if r3.StatusCode != http.StatusBadRequest {
		t.Errorf("empty next_steps: status = %d, want 400", r3.StatusCode)
	}

	// Happy path.
	r4 := postJSON(t, ts.URL+"/v1/handoffs/offer", map[string]any{
		"from_session":     "src-session-abc",
		"bootstrap_prompt": "x",
		"task":             map[string]any{"title": "T", "goal": "G", "next_steps": []any{"step1"}},
	})
	defer r4.Body.Close()
	if r4.StatusCode != http.StatusOK {
		t.Errorf("valid offer: status = %d, want 200", r4.StatusCode)
	}
}

// ─── 9. TestHandoffClaim_Atomicity ───────────────────────────────────────────

func TestHandoffClaim_Atomicity(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)

	// Post an offer first.
	offerResp := postJSON(t, ts.URL+"/v1/handoffs/offer", map[string]any{
		"from_session":     "offering-session-x",
		"bootstrap_prompt": "bp",
		"task":             map[string]any{"title": "T", "goal": "G", "next_steps": []any{"s1"}},
	})
	var offer handoffWriteResponse
	decodeJSON(t, offerResp, &offer)
	if !offer.OK {
		t.Fatalf("offer failed: %+v", offer)
	}
	claimURL := ts.URL + "/v1/handoffs/" + offer.HandoffID + "/claim"

	// Spin up 8 concurrent claims from 8 different sessions.
	const N = 8
	var wg sync.WaitGroup
	results := make([]int, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sess := []byte("claimant-session-0")
			sess[len(sess)-1] = byte('0' + idx)
			resp := postJSON(t, claimURL, map[string]any{"claiming_session": string(sess)})
			resp.Body.Close()
			results[idx] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	wins := 0
	losses := 0
	for _, code := range results {
		switch code {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
			losses++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if wins != 1 {
		t.Errorf("wins = %d, want 1", wins)
	}
	if losses != N-1 {
		t.Errorf("losses = %d, want %d", losses, N-1)
	}

	// Confirm the claim_rejected events landed for each loser.
	events, _ := srv.busSessions.ReadEvents(BusHandoffs)
	rejCount := 0
	for _, e := range events {
		if e.Type == EvtHandoffClaimRejected {
			rejCount++
		}
	}
	if rejCount != N-1 {
		t.Errorf("claim_rejected events = %d, want %d", rejCount, N-1)
	}
}

// ─── 10. TestHandoffClaim_TTLExpired ─────────────────────────────────────────

func TestHandoffClaim_TTLExpired(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)

	// Offer with 1s TTL, then backdate CreatedAt + ExpiresAt to force expiry.
	offerResp := postJSON(t, ts.URL+"/v1/handoffs/offer", map[string]any{
		"from_session":     "offering-session-y",
		"bootstrap_prompt": "bp",
		"ttl_seconds":      1,
		"task":             map[string]any{"title": "T", "goal": "G", "next_steps": []any{"s"}},
	})
	var offer handoffWriteResponse
	decodeJSON(t, offerResp, &offer)

	// Age the row.
	srv.handoffRegistry.mu.Lock()
	srv.handoffRegistry.rows[offer.HandoffID].CreatedAt =
		time.Now().Add(-1 * time.Hour)
	srv.handoffRegistry.rows[offer.HandoffID].ExpiresAt =
		time.Now().Add(-59 * time.Minute)
	srv.handoffRegistry.mu.Unlock()

	claimResp := postJSON(t, ts.URL+"/v1/handoffs/"+offer.HandoffID+"/claim",
		map[string]any{"claiming_session": "would-be-claimant"})
	defer claimResp.Body.Close()
	if claimResp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", claimResp.StatusCode)
	}
	body, _ := jsonReadAll(claimResp)
	if !strings.Contains(body, "ttl_expired") {
		t.Errorf("expected ttl_expired in body: %q", body)
	}
}

// ─── 11. TestHandoffClaim_PhantomOffer ───────────────────────────────────────

func TestHandoffClaim_PhantomOffer(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)

	resp := postJSON(t, ts.URL+"/v1/handoffs/ho-0-deadbeef/claim",
		map[string]any{"claiming_session": "phantom-claimant-abc"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	// claim_rejected event should still have been emitted for observability.
	events, _ := srv.busSessions.ReadEvents(BusHandoffs)
	found := false
	for _, e := range events {
		if e.Type == EvtHandoffClaimRejected {
			if r, _ := e.Payload["reason"].(string); r == string(ClaimRejectedOfferNotFound) {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("no claim_rejected/offer_not_found event on bus")
	}
}

// ─── 12. TestHandoffComplete_WithoutClaim ────────────────────────────────────

func TestHandoffComplete_WithoutClaim(t *testing.T) {
	t.Parallel()
	_, ts := newSessionsTestServer(t)

	offerResp := postJSON(t, ts.URL+"/v1/handoffs/offer", map[string]any{
		"from_session":     "offering-session-z",
		"bootstrap_prompt": "bp",
		"task":             map[string]any{"title": "T", "goal": "G", "next_steps": []any{"s"}},
	})
	var offer handoffWriteResponse
	decodeJSON(t, offerResp, &offer)

	resp := postJSON(t, ts.URL+"/v1/handoffs/"+offer.HandoffID+"/complete",
		map[string]any{
			"completing_session": "premature-completer-xyz",
			"outcome":            "done",
		})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// ─── 13. TestReplayOnStartup ─────────────────────────────────────────────────

func TestReplayOnStartup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog", Port: 0}
	nucleus := &Nucleus{Name: "test"}
	proc := NewProcess(cfg, nucleus)
	srv1 := NewServer(cfg, nucleus, proc)
	ts1 := httptest.NewServer(srv1.Handler())
	t.Cleanup(func() { ts1.Close() })

	// Register two sessions, end one, leave the other live.
	postJSON(t, ts1.URL+"/v1/sessions/register", map[string]any{
		"session_id": "replay-live-session", "workspace": "w", "role": "r",
	}).Body.Close()
	postJSON(t, ts1.URL+"/v1/sessions/register", map[string]any{
		"session_id": "replay-ended-session", "workspace": "w", "role": "r",
	}).Body.Close()
	postJSON(t, ts1.URL+"/v1/sessions/replay-ended-session/end",
		map[string]any{"reason": "test-end"}).Body.Close()

	// And a handoff.
	offerResp := postJSON(t, ts1.URL+"/v1/handoffs/offer", map[string]any{
		"from_session":     "replay-live-session",
		"bootstrap_prompt": "bp",
		"task":             map[string]any{"title": "T", "goal": "G", "next_steps": []any{"s"}},
	})
	var offer handoffWriteResponse
	decodeJSON(t, offerResp, &offer)

	ts1.Close()

	// Boot a fresh Server rooted at the same workspace — replay should
	// reconstruct the same state from the bus.
	srv2 := NewServer(cfg, nucleus, proc)
	if got := srv2.sessionRegistry.Len(); got != 2 {
		t.Errorf("replay session count = %d, want 2", got)
	}
	s1, ok := srv2.sessionRegistry.Get("replay-live-session")
	if !ok || s1.Ended {
		t.Errorf("live session not replayed correctly: ok=%v ended=%v", ok, s1 != nil && s1.Ended)
	}
	s2, ok := srv2.sessionRegistry.Get("replay-ended-session")
	var endReason string
	if s2 != nil {
		endReason = s2.EndReason
	}
	if !ok || s2 == nil || !s2.Ended || endReason != "test-end" {
		t.Errorf("ended session not replayed: ok=%v ended=%v reason=%q",
			ok, s2 != nil && s2.Ended, endReason)
	}
	if got := srv2.handoffRegistry.Len(); got != 1 {
		t.Errorf("replay handoff count = %d, want 1", got)
	}
	h, ok := srv2.handoffRegistry.Get(offer.HandoffID)
	var hState string
	if h != nil {
		hState = h.State
	}
	if !ok || h == nil || hState != HandoffStateOpen {
		t.Errorf("handoff not replayed as open: ok=%v state=%q", ok, hState)
	}
}

// ─── 14. TestClaimRejectedEventEmitted (amendment #4) ────────────────────────

func TestClaimRejectedEventEmitted(t *testing.T) {
	t.Parallel()
	srv, ts := newSessionsTestServer(t)

	// Post an offer; claim it once; then try to claim again. The second
	// attempt must 409 AND write a handoff.claim_rejected event with the
	// conflicting_session field populated.
	offerResp := postJSON(t, ts.URL+"/v1/handoffs/offer", map[string]any{
		"from_session":     "first-offerer-here",
		"bootstrap_prompt": "bp",
		"task":             map[string]any{"title": "T", "goal": "G", "next_steps": []any{"s"}},
	})
	var offer handoffWriteResponse
	decodeJSON(t, offerResp, &offer)

	// First claim wins.
	r1 := postJSON(t, ts.URL+"/v1/handoffs/"+offer.HandoffID+"/claim",
		map[string]any{"claiming_session": "first-winner-abc"})
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first claim: %d", r1.StatusCode)
	}

	// Second attempt loses.
	r2 := postJSON(t, ts.URL+"/v1/handoffs/"+offer.HandoffID+"/claim",
		map[string]any{"claiming_session": "second-loser-xyz"})
	r2.Body.Close()
	if r2.StatusCode != http.StatusConflict {
		t.Fatalf("second claim: %d, want 409", r2.StatusCode)
	}

	events, _ := srv.busSessions.ReadEvents(BusHandoffs)
	var rej *BusBlock
	for i := range events {
		if events[i].Type == EvtHandoffClaimRejected {
			rej = &events[i]
			break
		}
	}
	if rej == nil {
		t.Fatal("no claim_rejected event emitted")
	}
	if r, _ := rej.Payload["reason"].(string); r != string(ClaimRejectedAlreadyClaimed) {
		t.Errorf("reason = %q, want %q", r, ClaimRejectedAlreadyClaimed)
	}
	if cs, _ := rej.Payload["conflicting_session"].(string); cs != "first-winner-abc" {
		t.Errorf("conflicting_session = %q, want first-winner-abc", cs)
	}
	if as, _ := rej.Payload["attempting_session"].(string); as != "second-loser-xyz" {
		t.Errorf("attempting_session = %q, want second-loser-xyz", as)
	}
}

// ─── 15. TestMCP_HandoffRoundTrip — integration test (Phase 2) ─────────────

// TestMCP_HandoffRoundTrip exercises the 8 cog_* MCP tools end-to-end:
// register two sessions → one offers a handoff → the other claims it →
// claim emits the offer payload back → complete cycles the state to
// HandoffStateCompleted. Round trips the actual bus through the same
// registries the HTTP surface uses.
func TestMCP_HandoffRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog", Port: 0}
	proc := NewProcess(cfg, &Nucleus{Name: "test"})
	mcpSrv := NewMCPServer(cfg, &Nucleus{Name: "test"}, proc)
	bus := NewBusSessionManager(root)
	sessions := NewSessionRegistry()
	handoffs := NewHandoffRegistry()
	mcpSrv.SetSessionsBackend(bus, sessions, handoffs)

	ctx := mcpTestCtx(t)

	// 1. Register two sessions via the MCP tool.
	reg1Result, _, err := mcpSrv.toolRegisterSession(ctx, nil, registerSessionInput{
		SessionID: "mcp-src-session", Workspace: "w", Role: "r",
	})
	if err != nil || reg1Result == nil {
		t.Fatalf("register src: %v", err)
	}
	reg2Result, _, err := mcpSrv.toolRegisterSession(ctx, nil, registerSessionInput{
		SessionID: "mcp-dst-session", Workspace: "w", Role: "r",
	})
	if err != nil || reg2Result == nil {
		t.Fatalf("register dst: %v", err)
	}

	// 2. src offers a handoff.
	offerResult, _, err := mcpSrv.toolOfferHandoff(ctx, nil, offerHandoffInput{
		FromSession: "mcp-src-session",
		BootstrapPrompt: "bp",
		Task: map[string]interface{}{
			"title": "T", "goal": "G", "next_steps": []interface{}{"step1"},
		},
	})
	if err != nil || offerResult == nil {
		t.Fatalf("offer: %v", err)
	}
	var offerResp struct {
		OK        bool   `json:"ok"`
		HandoffID string `json:"handoff_id"`
	}
	decodeMCPJSON(t, offerResult, &offerResp)
	if !offerResp.OK || offerResp.HandoffID == "" {
		t.Fatalf("offer payload: %+v", offerResp)
	}

	// 3. dst claims it.
	claimResult, _, err := mcpSrv.toolClaimHandoff(ctx, nil, claimHandoffInput{
		HandoffID: offerResp.HandoffID, ClaimingSession: "mcp-dst-session",
	})
	if err != nil || claimResult == nil {
		t.Fatalf("claim: %v", err)
	}
	var claimResp struct {
		OK        bool                   `json:"ok"`
		HandoffID string                 `json:"handoff_id"`
		Offer     map[string]interface{} `json:"offer"`
	}
	decodeMCPJSON(t, claimResult, &claimResp)
	if !claimResp.OK || claimResp.HandoffID != offerResp.HandoffID {
		t.Fatalf("claim resp: %+v", claimResp)
	}
	if claimResp.Offer == nil || claimResp.Offer["bootstrap_prompt"] != "bp" {
		t.Errorf("claim did not surface bootstrap_prompt: %+v", claimResp.Offer)
	}

	// 4. dst completes the handoff.
	compResult, _, err := mcpSrv.toolCompleteHandoff(ctx, nil, completeHandoffInput{
		HandoffID: offerResp.HandoffID, CompletingSession: "mcp-dst-session",
		Outcome: "success", Notes: "roundtrip done",
	})
	if err != nil || compResult == nil {
		t.Fatalf("complete: %v", err)
	}
	var compResp struct {
		OK bool `json:"ok"`
	}
	decodeMCPJSON(t, compResult, &compResp)
	if !compResp.OK {
		t.Errorf("complete not ok: %+v", compResp)
	}

	// Verify bus has the full chain: 1 offer + 1 claim + 1 complete.
	events, _ := bus.ReadEvents(BusHandoffs)
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	wantSeq := []string{EvtHandoffOffer, EvtHandoffClaim, EvtHandoffComplete}
	if len(events) != 3 {
		t.Fatalf("bus chain length = %d, want 3: %v", len(events), types)
	}
	for i, e := range events {
		if e.Type != wantSeq[i] {
			t.Errorf("event[%d].type = %q, want %q", i, e.Type, wantSeq[i])
		}
	}

	// In-memory state: handoff should be in completed state.
	h, ok := handoffs.Get(offerResp.HandoffID)
	if !ok || h.State != HandoffStateCompleted {
		var state string
		if h != nil {
			state = h.State
		}
		t.Errorf("final handoff state = %q (ok=%v), want completed", state, ok)
	}
}

// mcpTestCtx returns a background context with t.Deadline if set.
func mcpTestCtx(t *testing.T) context.Context {
	ctx := t.Context()
	return ctx
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func jsonReadAll(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	b, err := readAll(resp.Body)
	return string(b), err
}

func readAll(r interface {
	Read(p []byte) (int, error)
}) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 512)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
