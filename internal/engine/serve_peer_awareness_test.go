// serve_peer_awareness_test.go — HTTP-level tests for /v1/peer-awareness.
//
// Uses a live Server wired with a temp workspace so the bus/attention log
// paths are real, then seeds data directly through BusSessionManager and
// the attention JSONL file. Exercises the happy path, default values,
// 400 on bad input, and the end-to-end MCP contract shape.

package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newPeerAwarenessTestServer builds a minimal Server with a real bus and
// handoff registry on a temp workspace. Exposed helpers seed the three
// data sources the composer reads (activity channels, attention.jsonl,
// bus_handoffs + bus_broadcast).
func newPeerAwarenessTestServer(t *testing.T) (http.Handler, *Server, string) {
	t.Helper()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: filepath.Join(root, ".cog"), Port: 0}
	nucleus := &Nucleus{Name: "test"}
	proc := NewProcess(cfg, nucleus)
	srv := NewServer(cfg, nucleus, proc)
	t.Cleanup(func() { _ = proc.Broker().Close() })
	return srv.Handler(), srv, root
}

// seedActivity appends a tailer.block event onto channel.<sid>.activity.
func seedActivity(t *testing.T, s *Server, sid string, ts time.Time, kind, source, ref string) {
	t.Helper()
	_, err := s.busSessions.AppendEvent(ActivityChannelForSid(sid), evtTailerBlock, source, map[string]interface{}{
		"kind":           kind,
		"source_channel": source,
		"timestamp":      ts.UTC().Format(time.RFC3339Nano),
		"ref":            ref,
	})
	if err != nil {
		t.Fatalf("seed activity: %v", err)
	}
}

// seedAttention writes one line into .cog/run/attention.jsonl.
func seedAttention(t *testing.T, root, participantID, targetURI string, ts time.Time) {
	t.Helper()
	dir := filepath.Join(root, ".cog", "run")
	_ = os.MkdirAll(dir, 0755)
	f, err := os.OpenFile(filepath.Join(dir, "attention.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open attention.jsonl: %v", err)
	}
	defer f.Close()
	line := fmt.Sprintf(`{"participant_id":%q,"target_uri":%q,"signal_type":"visit","occurred_at":%q}`+"\n",
		participantID, targetURI, ts.UTC().Format(time.RFC3339))
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("write attention: %v", err)
	}
}

// ─── happy-path tests ────────────────────────────────────────────────────────

// TestHTTPPeerAwarenessEmpty: a well-formed sid with no underlying data
// returns 200 + packet="" + sources=[]. Load-bearing — the contract says
// 503 is for dependency outage, not empty packets.
func TestHTTPPeerAwarenessEmpty(t *testing.T) {
	t.Parallel()
	handler, _, _ := newPeerAwarenessTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/peer-awareness?sid=alpha-beta-gamma")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, body)
	}

	var res PeerAwarenessResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Packet != "" {
		t.Errorf("packet = %q, want empty", res.Packet)
	}
	if len(res.Sources) != 0 {
		t.Errorf("sources len = %d, want 0", len(res.Sources))
	}
	if res.TokenCount != 0 {
		t.Errorf("token_count = %d, want 0", res.TokenCount)
	}
}

// TestHTTPPeerAwarenessWithData: seeds all four sections and verifies
// the packet contains representative text from each + sources match up.
func TestHTTPPeerAwarenessWithData(t *testing.T) {
	t.Parallel()
	handler, s, root := newPeerAwarenessTestServer(t)
	me := "alpha-beta-gamma"
	peer := "peer-session-one"
	now := time.Now().UTC()

	seedActivity(t, s, me, now.Add(-3*time.Minute), "assistant", "claude-code:conv-1", "ref-mine")
	seedActivity(t, s, peer, now.Add(-2*time.Minute), "user", "claude-code:conv-2", "ref-peer")

	seedAttention(t, root, me, "cog://mem/shared", now.Add(-4*time.Minute))
	seedAttention(t, root, peer, "cog://mem/shared", now.Add(-1*time.Minute))

	// Handoff where `me` is the source.
	_, _ = s.handoffRegistry.ApplyOffer(HandoffState{
		HandoffID:   "ho-test-1",
		FromSession: me,
		ToSession:   peer,
		Reason:      "rolling test",
		CreatedAt:   now.Add(-5 * time.Minute),
	}, now.Add(-5*time.Minute), nil)

	// coord.* on bus_broadcast.
	_, _ = s.busSessions.AppendEvent(BusBroadcast, "coord.standup", "peer:p1", map[string]interface{}{"summary": "tracking-progress"})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/peer-awareness?sid=" + me + "&budget=1000")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var res PeerAwarenessResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Each section should show.
	for _, want := range []string{"MY RECENT ACTIVITY", "OPEN HANDOFFS", "PEER OVERLAP", "COORD CHATTER"} {
		if !strings.Contains(res.Packet, want) {
			t.Errorf("missing section %q\nPacket:\n%s", want, res.Packet)
		}
	}

	// And every source entry must have type+ts populated.
	for i, s := range res.Sources {
		if s.Type == "" || s.Ts == "" {
			t.Errorf("sources[%d] missing type/ts: %+v", i, s)
		}
	}
	// Notes carries the MVP flag.
	if len(res.Notes) == 0 || res.Notes[0] != "anti_echo_mvp" {
		t.Errorf("notes = %v, want [anti_echo_mvp]", res.Notes)
	}
}

// TestHTTPPeerAwarenessDefaultBudget: no budget in query → 500 token default.
// Spot check that a response fits within an order-of-magnitude of 500 tokens.
func TestHTTPPeerAwarenessDefaultBudget(t *testing.T) {
	t.Parallel()
	handler, s, _ := newPeerAwarenessTestServer(t)
	sid := "alpha-beta-gamma"
	now := time.Now().UTC()
	for i := 0; i < 20; i++ {
		seedActivity(t, s, sid,
			now.Add(-time.Duration(i+1)*time.Minute),
			"assistant",
			"claude-code:conv-"+fmt.Sprint(i),
			fmt.Sprintf("ref-%d", i))
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/peer-awareness?sid=" + sid)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var res PeerAwarenessResult
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if res.TokenCount > DefaultPeerAwarenessBudget {
		t.Errorf("token_count = %d, want <= %d", res.TokenCount, DefaultPeerAwarenessBudget)
	}
}

// TestHTTPPeerAwarenessIncludePeersFalse: flag suppresses peer section.
func TestHTTPPeerAwarenessIncludePeersFalse(t *testing.T) {
	t.Parallel()
	handler, s, root := newPeerAwarenessTestServer(t)
	me := "alpha-beta-gamma"
	peer := "peer-session-one"
	now := time.Now().UTC()

	seedActivity(t, s, peer, now.Add(-2*time.Minute), "user", "src", "ref-peer")
	seedAttention(t, root, me, "cog://mem/shared", now.Add(-3*time.Minute))
	seedAttention(t, root, peer, "cog://mem/shared", now.Add(-1*time.Minute))

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/peer-awareness?sid=" + me + "&include_peers=false")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var res PeerAwarenessResult
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if strings.Contains(res.Packet, "PEER OVERLAP") {
		t.Errorf("peer overlap appeared despite include_peers=false:\n%s", res.Packet)
	}
}

// ─── error paths ─────────────────────────────────────────────────────────────

// TestHTTPPeerAwarenessBadSid exhausts the 400-mapping:
//   - missing sid
//   - path-char sid
//   - malformed budget
//   - malformed window
//   - malformed include_peers
func TestHTTPPeerAwarenessBadSid(t *testing.T) {
	t.Parallel()
	handler, _, _ := newPeerAwarenessTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cases := []struct {
		name string
		url  string
	}{
		{"missing", srv.URL + "/v1/peer-awareness"},
		{"empty", srv.URL + "/v1/peer-awareness?sid="},
		{"path-chars", srv.URL + "/v1/peer-awareness?sid=" + strings.ReplaceAll("a/b/c", "/", "%2F")},
		{"uppercase", srv.URL + "/v1/peer-awareness?sid=A-B-C"},
		{"bad-budget", srv.URL + "/v1/peer-awareness?sid=alpha-beta-gamma&budget=notanint"},
		{"bad-window", srv.URL + "/v1/peer-awareness?sid=alpha-beta-gamma&window=thirty-tacos"},
		{"bad-include-peers", srv.URL + "/v1/peer-awareness?sid=alpha-beta-gamma&include_peers=maybe"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := http.Get(tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want 400; body=%s", resp.StatusCode, body)
			}
		})
	}
}

// TestHTTPPeerAwarenessBeaconEmitted: after a successful render, the
// causality bus bus_peer_awareness should have a fresh
// peer_awareness.rendered event.
func TestHTTPPeerAwarenessBeaconEmitted(t *testing.T) {
	t.Parallel()
	handler, s, _ := newPeerAwarenessTestServer(t)
	sid := "alpha-beta-gamma"
	now := time.Now().UTC()
	seedActivity(t, s, sid, now.Add(-1*time.Minute), "assistant", "src", "ref")

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/peer-awareness?sid=" + sid)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	events, err := s.busSessions.ReadEvents(PeerAwarenessRenderBus)
	if err != nil {
		t.Fatalf("read beacon bus: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("no beacon events on %s", PeerAwarenessRenderBus)
	}
	var matched bool
	for _, e := range events {
		if e.Type == EvtPeerAwarenessRendered {
			matched = true
			if e.Payload["sid"] != sid {
				t.Errorf("beacon sid = %v, want %q", e.Payload["sid"], sid)
			}
		}
	}
	if !matched {
		t.Errorf("no %s event on bus; got %+v", EvtPeerAwarenessRendered, events)
	}
}

// TestHTTPPeerAwarenessResponseContentType: contract detail — response is
// always application/json, even for 4xx.
func TestHTTPPeerAwarenessResponseContentType(t *testing.T) {
	t.Parallel()
	handler, _, _ := newPeerAwarenessTestServer(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	for _, url := range []string{
		srv.URL + "/v1/peer-awareness?sid=alpha-beta-gamma",
		srv.URL + "/v1/peer-awareness", // 400
	} {
		resp, err := http.Get(url)
		if err != nil {
			t.Errorf("GET %s: %v", url, err)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("content-type for %s = %q", url, ct)
		}
		resp.Body.Close()
	}
}
