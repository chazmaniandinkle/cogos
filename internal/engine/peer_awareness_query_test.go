// peer_awareness_query_test.go — unit tests for the packet composer.
//
// Tests use in-memory fake implementations of the deps bundle so the
// composer can be exercised without a live BusSessionManager / workspace.
// Each test names one contract expectation in its header comment.

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// fakeBusReader is a minimal bus used by the composer. Keyed by bus_id;
// returns the per-bus slice unchanged.
type fakeBusReader struct {
	data     map[string][]BusBlock
	appended []appendedEvt
}

func newFakeBus() *fakeBusReader {
	return &fakeBusReader{data: map[string][]BusBlock{}}
}

func (f *fakeBusReader) ReadEvents(busID string) ([]BusBlock, error) {
	return f.data[busID], nil
}

type appendedEvt struct {
	busID, eventType, from string
	payload                map[string]interface{}
}

// AppendEvent satisfies peerAwarenessRenderEmitter so the composer's
// beacon emission is observable.
func (f *fakeBusReader) AppendEvent(busID, eventType, from string, payload map[string]interface{}) (*BusBlock, error) {
	f.appended = append(f.appended, appendedEvt{busID, eventType, from, payload})
	seq := len(f.data[busID]) + 1
	block := BusBlock{
		V:       2,
		BusID:   busID,
		Seq:     seq,
		Ts:      time.Now().UTC().Format(time.RFC3339Nano),
		From:    from,
		Type:    eventType,
		Payload: payload,
	}
	f.data[busID] = append(f.data[busID], block)
	return &block, nil
}

// fakeAttn returns a fixed list of signals.
type fakeAttn struct {
	signals []PeerAwarenessAttentionSignal
}

func (f *fakeAttn) RecentSignals(n int) []PeerAwarenessAttentionSignal {
	if n >= len(f.signals) {
		return f.signals
	}
	return f.signals[:n]
}

// fakeHandoffs wraps a slice so tests control the Snapshot shape.
type fakeHandoffs struct {
	rows []*HandoffState
}

func (f *fakeHandoffs) Snapshot() []*HandoffState {
	// Return copies to match the real registry's behaviour.
	out := make([]*HandoffState, len(f.rows))
	for i, r := range f.rows {
		if r == nil {
			continue
		}
		cp := *r
		out[i] = &cp
	}
	return out
}

// buildTailerEvent synthesizes a tailer.block bus entry matching Phase 1A's
// on-wire shape. Tests use this to seed the activity channels.
func buildTailerEvent(sid string, seq int, ts time.Time, kind, source, ref string) BusBlock {
	return BusBlock{
		V:     2,
		BusID: ActivityChannelForSid(sid),
		Seq:   seq,
		Ts:    ts.UTC().Format(time.RFC3339Nano),
		From:  source,
		Type:  evtTailerBlock,
		Payload: map[string]interface{}{
			"kind":           kind,
			"source_channel": source,
			"timestamp":      ts.UTC().Format(time.RFC3339Nano),
			"ref":            ref,
		},
	}
}

// ─── ValidateSid ─────────────────────────────────────────────────────────────

func TestValidateSid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		sid     string
		wantErr bool
	}{
		{"empty", "", true},
		{"simple", "alpha-beta-gamma", false},
		{"short", "a-b-c", false},
		{"slash", "a/b/c", true},
		{"backslash", "a\\b\\c", true},
		{"space", "a b c", true},
		{"newline", "a\nb", true},
		{"double-hyphen", "a--b-c", true},
		{"uppercase", "A-B-C", true},
		{"trailing-hyphen", "a-b-", true},
		{"leading-hyphen", "-a-b-c", true},
		{"null-byte", "a\x00b", true},
		{"too-long", strings.Repeat("a", 129), true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSid(tc.sid)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateSid(%q) err=%v, wantErr=%v", tc.sid, err, tc.wantErr)
			}
		})
	}
}

// ─── core rendering ──────────────────────────────────────────────────────────

// TestRenderEmptyWhenNoData asserts the graceful-empty contract: an sid
// with no backing data yields a 200-compatible result (empty packet, no
// sources, note "anti_echo_mvp"). Load-bearing — the HTTP handler reports
// this as 200 not 503.
func TestRenderEmptyWhenNoData(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	deps := peerAwarenessDeps{bus: bus, attn: &fakeAttn{}, handoffs: &fakeHandoffs{}, renderer: bus}
	req := PeerAwarenessRequest{Sid: "alpha-beta-gamma", IncludePeers: true, Now: time.Now().UTC()}

	res, err := RenderPeerAwarenessPacket(deps, req)
	if err != nil {
		t.Fatalf("render err=%v", err)
	}
	if res.Packet != "" {
		t.Errorf("Packet = %q, want empty", res.Packet)
	}
	if len(res.Sources) != 0 {
		t.Errorf("Sources len=%d, want 0", len(res.Sources))
	}
	if res.TokenCount != 0 {
		t.Errorf("TokenCount = %d, want 0", res.TokenCount)
	}
	if len(res.Notes) == 0 || res.Notes[0] != "anti_echo_mvp" {
		t.Errorf("Notes = %v, want [anti_echo_mvp]", res.Notes)
	}
	// Beacon is always emitted — even for empty packets so the causality
	// chain has a clean "I looked at this sid at T" marker.
	if len(bus.appended) != 1 {
		t.Fatalf("beacon count = %d, want 1", len(bus.appended))
	}
	if bus.appended[0].busID != PeerAwarenessRenderBus || bus.appended[0].eventType != EvtPeerAwarenessRendered {
		t.Errorf("beacon = %+v, want %s/%s", bus.appended[0], PeerAwarenessRenderBus, EvtPeerAwarenessRendered)
	}
}

// TestRenderMyActivity populates the session's own activity channel and
// expects the section to appear in the packet, with one line per event
// and a source[] entry per event.
func TestRenderMyActivity(t *testing.T) {
	t.Parallel()
	sid := "alpha-beta-gamma"
	now := time.Date(2026, 4, 23, 15, 0, 0, 0, time.UTC)

	bus := newFakeBus()
	bus.data[ActivityChannelForSid(sid)] = []BusBlock{
		buildTailerEvent(sid, 1, now.Add(-10*time.Minute), "assistant", "claude-code:conv-42", "h1"),
		buildTailerEvent(sid, 2, now.Add(-3*time.Minute), "user", "claude-code:conv-42", "h2"),
	}
	deps := peerAwarenessDeps{bus: bus, attn: &fakeAttn{}, handoffs: &fakeHandoffs{}, renderer: bus}
	req := PeerAwarenessRequest{Sid: sid, Budget: 500, Window: 15 * time.Minute, IncludePeers: true, Now: now}

	res, err := RenderPeerAwarenessPacket(deps, req)
	if err != nil {
		t.Fatalf("render err=%v", err)
	}
	if !strings.Contains(res.Packet, "MY RECENT ACTIVITY") {
		t.Errorf("missing heading\n%s", res.Packet)
	}
	if !strings.Contains(res.Packet, "assistant") || !strings.Contains(res.Packet, "user") {
		t.Errorf("missing events\n%s", res.Packet)
	}
	if res.TokenCount <= 0 {
		t.Errorf("TokenCount = %d, want >0", res.TokenCount)
	}
	// Count my_activity-tagged sources.
	var myCount int
	for _, s := range res.Sources {
		if s.Section == "my_activity" {
			myCount++
		}
	}
	if myCount != 2 {
		t.Errorf("my_activity sources = %d, want 2", myCount)
	}
}

// TestRenderMyActivityWindowFilter confirms events older than the window
// are dropped. 30-minute-old events must not contribute to a 15-minute query.
func TestRenderMyActivityWindowFilter(t *testing.T) {
	t.Parallel()
	sid := "alpha-beta-gamma"
	now := time.Date(2026, 4, 23, 15, 0, 0, 0, time.UTC)

	bus := newFakeBus()
	bus.data[ActivityChannelForSid(sid)] = []BusBlock{
		buildTailerEvent(sid, 1, now.Add(-30*time.Minute), "assistant", "src", "old"), // OUT of window
		buildTailerEvent(sid, 2, now.Add(-5*time.Minute), "user", "src", "fresh"),     // IN window
	}
	deps := peerAwarenessDeps{bus: bus, attn: &fakeAttn{}, handoffs: &fakeHandoffs{}, renderer: bus}
	req := PeerAwarenessRequest{Sid: sid, Window: 15 * time.Minute, IncludePeers: true, Now: now}

	res, _ := RenderPeerAwarenessPacket(deps, req)
	if strings.Contains(res.Packet, "old") {
		t.Errorf("old ref leaked into packet:\n%s", res.Packet)
	}
	if !strings.Contains(res.Packet, "MY RECENT ACTIVITY") {
		t.Errorf("expected activity section\n%s", res.Packet)
	}
	my := 0
	for _, s := range res.Sources {
		if s.Section == "my_activity" {
			my++
		}
	}
	if my != 1 {
		t.Errorf("my_activity sources = %d, want 1", my)
	}
}

// TestRenderHandoffs verifies handoffs where the sid is the originating
// session render, with state + counterparty in the line. Only handoffs
// touched within the window appear.
func TestRenderHandoffs(t *testing.T) {
	t.Parallel()
	sid := "alpha-beta-gamma"
	now := time.Date(2026, 4, 23, 15, 0, 0, 0, time.UTC)

	ho := &fakeHandoffs{rows: []*HandoffState{
		{
			HandoffID:   "ho-1",
			FromSession: sid,
			ToSession:   "peer-session-one",
			Reason:      "rolling handoff",
			CreatedAt:   now.Add(-10 * time.Minute),
			State:       HandoffStateOpen,
		},
		{
			HandoffID:       "ho-2",
			FromSession:     "someone-else-entirely",
			ClaimingSession: sid,
			Reason:          "claimed by me",
			CreatedAt:       now.Add(-20 * time.Minute),
			ClaimedAt:       now.Add(-4 * time.Minute),
			State:           HandoffStateClaimed,
		},
		{
			HandoffID:   "ho-stale",
			FromSession: sid,
			CreatedAt:   now.Add(-2 * time.Hour), // outside window
			State:       HandoffStateCompleted,
		},
	}}
	bus := newFakeBus()
	deps := peerAwarenessDeps{bus: bus, attn: &fakeAttn{}, handoffs: ho, renderer: bus}
	req := PeerAwarenessRequest{Sid: sid, Window: 15 * time.Minute, IncludePeers: true, Now: now}

	res, _ := RenderPeerAwarenessPacket(deps, req)
	if !strings.Contains(res.Packet, "OPEN HANDOFFS") {
		t.Fatalf("missing OPEN HANDOFFS heading:\n%s", res.Packet)
	}
	if !strings.Contains(res.Packet, "peer-session-one") {
		t.Errorf("expected counterparty in line:\n%s", res.Packet)
	}
	if strings.Contains(res.Packet, "ho-stale") {
		t.Errorf("stale handoff must not render:\n%s", res.Packet)
	}

	// Sources — filter to handoff section.
	var hc int
	for _, s := range res.Sources {
		if s.Section == "handoffs" {
			hc++
			if s.BusID != BusHandoffs {
				t.Errorf("handoff source bus_id = %q, want %q", s.BusID, BusHandoffs)
			}
		}
	}
	if hc != 2 {
		t.Errorf("handoff sources = %d, want 2", hc)
	}
}

// TestRenderPeerOverlap is the meat of the 4E loop: two peers sharing an
// attention target plus the peer's own activity channel contributes to
// the overlap section. Explicit assertion that the peer's activity line
// is the one pulled into the packet.
func TestRenderPeerOverlap(t *testing.T) {
	t.Parallel()
	me := "alpha-beta-gamma"
	peer := "peer-session-one"
	now := time.Date(2026, 4, 23, 15, 0, 0, 0, time.UTC)

	bus := newFakeBus()
	// Peer's activity channel — one recent event, one old (out of window).
	bus.data[ActivityChannelForSid(peer)] = []BusBlock{
		buildTailerEvent(peer, 1, now.Add(-30*time.Minute), "assistant", "peer:old", "peer-stale"),
		buildTailerEvent(peer, 2, now.Add(-4*time.Minute), "user", "peer:fresh", "peer-fresh"),
	}

	attn := &fakeAttn{signals: []PeerAwarenessAttentionSignal{
		{ParticipantID: me, TargetURI: "cog://mem/shared", SignalType: "visit", OccurredAt: now.Add(-5 * time.Minute).Format(time.RFC3339)},
		{ParticipantID: peer, TargetURI: "cog://mem/shared", SignalType: "visit", OccurredAt: now.Add(-2 * time.Minute).Format(time.RFC3339)},
		// Target only I look at — should not create overlap.
		{ParticipantID: me, TargetURI: "cog://mem/solo", SignalType: "visit", OccurredAt: now.Add(-3 * time.Minute).Format(time.RFC3339)},
	}}
	deps := peerAwarenessDeps{bus: bus, attn: attn, handoffs: &fakeHandoffs{}, renderer: bus}
	req := PeerAwarenessRequest{Sid: me, Window: 15 * time.Minute, IncludePeers: true, Now: now, Budget: 800}

	res, _ := RenderPeerAwarenessPacket(deps, req)
	if !strings.Contains(res.Packet, "PEER OVERLAP") {
		t.Fatalf("missing PEER OVERLAP heading:\n%s", res.Packet)
	}
	if !strings.Contains(res.Packet, peer) {
		t.Errorf("missing peer sid in packet:\n%s", res.Packet)
	}
	if !strings.Contains(res.Packet, "shared") {
		t.Errorf("expected shared target in overlap line:\n%s", res.Packet)
	}
	if !strings.Contains(res.Packet, "peer:fresh") {
		t.Errorf("expected peer's fresh activity line:\n%s", res.Packet)
	}
	if strings.Contains(res.Packet, "peer-stale") || strings.Contains(res.Packet, "peer:old") {
		t.Errorf("stale peer event must not render:\n%s", res.Packet)
	}

	// Source entry for the peer, tagged as peer_activity.
	var peerSrc *PeerAwarenessSource
	for i := range res.Sources {
		if res.Sources[i].Section == "peer_activity" && res.Sources[i].Sid == peer {
			peerSrc = &res.Sources[i]
			break
		}
	}
	if peerSrc == nil {
		t.Fatalf("no peer_activity source for peer %q; sources=%+v", peer, res.Sources)
	}
	if peerSrc.Ref != "peer-fresh" {
		t.Errorf("peer source ref = %q, want peer-fresh", peerSrc.Ref)
	}
}

// TestRenderIncludePeersFalse suppresses section 3 entirely when the flag
// is off — confirms the contract knob works end-to-end.
func TestRenderIncludePeersFalse(t *testing.T) {
	t.Parallel()
	me := "alpha-beta-gamma"
	peer := "peer-session-one"
	now := time.Date(2026, 4, 23, 15, 0, 0, 0, time.UTC)

	bus := newFakeBus()
	bus.data[ActivityChannelForSid(peer)] = []BusBlock{
		buildTailerEvent(peer, 1, now.Add(-4*time.Minute), "assistant", "peer:fresh", "peer-fresh"),
	}
	attn := &fakeAttn{signals: []PeerAwarenessAttentionSignal{
		{ParticipantID: me, TargetURI: "cog://mem/shared", SignalType: "visit", OccurredAt: now.Add(-5 * time.Minute).Format(time.RFC3339)},
		{ParticipantID: peer, TargetURI: "cog://mem/shared", SignalType: "visit", OccurredAt: now.Add(-2 * time.Minute).Format(time.RFC3339)},
	}}
	deps := peerAwarenessDeps{bus: bus, attn: attn, handoffs: &fakeHandoffs{}, renderer: bus}
	req := PeerAwarenessRequest{Sid: me, Window: 15 * time.Minute, IncludePeers: false, Now: now}

	res, _ := RenderPeerAwarenessPacket(deps, req)
	if strings.Contains(res.Packet, "PEER OVERLAP") {
		t.Fatalf("peer overlap must be suppressed when include_peers=false:\n%s", res.Packet)
	}
	for _, s := range res.Sources {
		if s.Section == "peer_activity" {
			t.Errorf("peer_activity source leaked:\n%+v", s)
		}
	}
}

// TestRenderCoordEvents: coord.*/impl.* events on bus_broadcast render in
// section 4; unrelated types on bus_broadcast are filtered out.
func TestRenderCoordEvents(t *testing.T) {
	t.Parallel()
	sid := "alpha-beta-gamma"
	now := time.Date(2026, 4, 23, 15, 0, 0, 0, time.UTC)

	bus := newFakeBus()
	bus.data[BusBroadcast] = []BusBlock{
		{V: 2, BusID: BusBroadcast, Seq: 1, Ts: now.Add(-6 * time.Minute).Format(time.RFC3339Nano), From: "peer:p1", Type: "coord.standup", Payload: map[string]interface{}{"summary": "working on X"}},
		{V: 2, BusID: BusBroadcast, Seq: 2, Ts: now.Add(-2 * time.Minute).Format(time.RFC3339Nano), From: "peer:p2", Type: "impl.merged", Payload: map[string]interface{}{"summary": "green on main"}},
		{V: 2, BusID: BusBroadcast, Seq: 3, Ts: now.Add(-1 * time.Minute).Format(time.RFC3339Nano), From: "peer:p3", Type: "chat.chatter", Payload: map[string]interface{}{"summary": "ignored"}},
	}
	deps := peerAwarenessDeps{bus: bus, attn: &fakeAttn{}, handoffs: &fakeHandoffs{}, renderer: bus}
	req := PeerAwarenessRequest{Sid: sid, Window: 15 * time.Minute, IncludePeers: true, Now: now, Budget: 800}

	res, _ := RenderPeerAwarenessPacket(deps, req)
	if !strings.Contains(res.Packet, "COORD CHATTER") {
		t.Fatalf("missing COORD CHATTER heading:\n%s", res.Packet)
	}
	if !strings.Contains(res.Packet, "coord.standup") || !strings.Contains(res.Packet, "impl.merged") {
		t.Errorf("expected coord+impl events:\n%s", res.Packet)
	}
	if strings.Contains(res.Packet, "chat.chatter") {
		t.Errorf("unrelated bus_broadcast type leaked:\n%s", res.Packet)
	}
}

// TestBudgetTruncation ensures the packet stays under the requested
// token budget even when every section has data.
func TestBudgetTruncation(t *testing.T) {
	t.Parallel()
	sid := "alpha-beta-gamma"
	now := time.Date(2026, 4, 23, 15, 0, 0, 0, time.UTC)

	bus := newFakeBus()
	var events []BusBlock
	for i := 0; i < 50; i++ {
		events = append(events, buildTailerEvent(
			sid, i+1, now.Add(-time.Duration(i)*time.Minute),
			"assistant",
			fmt.Sprintf("claude-code:conv-%d-with-a-very-long-identifier", i),
			fmt.Sprintf("ref-%d", i),
		))
	}
	bus.data[ActivityChannelForSid(sid)] = events

	deps := peerAwarenessDeps{bus: bus, attn: &fakeAttn{}, handoffs: &fakeHandoffs{}, renderer: bus}
	req := PeerAwarenessRequest{Sid: sid, Budget: 40, Window: 2 * time.Hour, IncludePeers: true, Now: now}

	res, _ := RenderPeerAwarenessPacket(deps, req)
	if res.TokenCount > 40 {
		t.Errorf("TokenCount = %d, want <= 40; packet:\n%s", res.TokenCount, res.Packet)
	}
	if res.Packet == "" {
		t.Errorf("expected a non-empty packet within budget")
	}
}

// TestBeaconPayload: the peer_awareness.rendered event carries the sid
// and the list of peers we mentioned. Used by the anti-echo follow-up.
func TestBeaconPayload(t *testing.T) {
	t.Parallel()
	me := "alpha-beta-gamma"
	peer := "peer-session-one"
	now := time.Date(2026, 4, 23, 15, 0, 0, 0, time.UTC)

	bus := newFakeBus()
	bus.data[ActivityChannelForSid(peer)] = []BusBlock{
		buildTailerEvent(peer, 1, now.Add(-4*time.Minute), "user", "peer:fresh", "ref-fresh"),
	}
	attn := &fakeAttn{signals: []PeerAwarenessAttentionSignal{
		{ParticipantID: me, TargetURI: "cog://mem/shared", SignalType: "visit", OccurredAt: now.Add(-5 * time.Minute).Format(time.RFC3339)},
		{ParticipantID: peer, TargetURI: "cog://mem/shared", SignalType: "visit", OccurredAt: now.Add(-2 * time.Minute).Format(time.RFC3339)},
	}}
	deps := peerAwarenessDeps{bus: bus, attn: attn, handoffs: &fakeHandoffs{}, renderer: bus}
	req := PeerAwarenessRequest{Sid: me, Window: 15 * time.Minute, IncludePeers: true, Now: now, Budget: 500}

	_, err := RenderPeerAwarenessPacket(deps, req)
	if err != nil {
		t.Fatalf("render err=%v", err)
	}
	var rendered *appendedEvt
	for i := range bus.appended {
		if bus.appended[i].eventType == EvtPeerAwarenessRendered {
			rendered = &bus.appended[i]
			break
		}
	}
	if rendered == nil {
		t.Fatalf("no beacon emitted; appended=%+v", bus.appended)
	}
	if got := rendered.payload["sid"]; got != me {
		t.Errorf("beacon sid = %v, want %q", got, me)
	}
	peers, ok := rendered.payload["peers"].([]string)
	if !ok {
		t.Fatalf("beacon peers wrong type: %T", rendered.payload["peers"])
	}
	if len(peers) != 1 || peers[0] != peer {
		t.Errorf("beacon peers = %v, want [%s]", peers, peer)
	}
}

// TestEchoRiskTagging: after a prior peer_awareness.rendered event
// mentioned a peer, a subsequent render flags that peer's source with
// echo_risk=true. MVP tag — no subtractive filter.
func TestEchoRiskTagging(t *testing.T) {
	t.Parallel()
	me := "alpha-beta-gamma"
	peer := "peer-session-one"
	now := time.Date(2026, 4, 23, 15, 0, 0, 0, time.UTC)

	bus := newFakeBus()
	// Prior render beacon: me already mentioned peer once.
	bus.data[PeerAwarenessRenderBus] = []BusBlock{
		{
			V:     2,
			BusID: PeerAwarenessRenderBus,
			Seq:   1,
			Ts:    now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
			From:  me,
			Type:  EvtPeerAwarenessRendered,
			Payload: map[string]interface{}{
				"sid":   me,
				"peers": []interface{}{peer},
			},
		},
	}
	bus.data[ActivityChannelForSid(peer)] = []BusBlock{
		buildTailerEvent(peer, 1, now.Add(-1*time.Minute), "user", "peer:fresh", "ref-echo"),
	}
	attn := &fakeAttn{signals: []PeerAwarenessAttentionSignal{
		{ParticipantID: me, TargetURI: "cog://mem/shared", OccurredAt: now.Add(-90 * time.Second).Format(time.RFC3339)},
		{ParticipantID: peer, TargetURI: "cog://mem/shared", OccurredAt: now.Add(-30 * time.Second).Format(time.RFC3339)},
	}}
	deps := peerAwarenessDeps{bus: bus, attn: attn, handoffs: &fakeHandoffs{}, renderer: bus}
	req := PeerAwarenessRequest{Sid: me, Window: 15 * time.Minute, IncludePeers: true, Now: now, Budget: 800}

	res, _ := RenderPeerAwarenessPacket(deps, req)
	var peerSrc *PeerAwarenessSource
	for i := range res.Sources {
		if res.Sources[i].Section == "peer_activity" && res.Sources[i].Sid == peer {
			peerSrc = &res.Sources[i]
			break
		}
	}
	if peerSrc == nil {
		t.Fatalf("no peer_activity source for %q; got %+v", peer, res.Sources)
	}
	if !peerSrc.EchoRisk {
		t.Errorf("echo_risk = false; want true (prior beacon already mentioned %q)", peer)
	}
}

// TestInvalidSidRejected: RenderPeerAwarenessPacket re-validates sid and
// returns an error, not a half-rendered packet, when the sid is unsafe.
func TestInvalidSidRejected(t *testing.T) {
	t.Parallel()
	bus := newFakeBus()
	deps := peerAwarenessDeps{bus: bus, attn: &fakeAttn{}, handoffs: &fakeHandoffs{}, renderer: bus}

	bad := []string{"", "../escape", "a/b/c", "A-B-C", "has space"}
	for _, sid := range bad {
		_, err := RenderPeerAwarenessPacket(deps, PeerAwarenessRequest{Sid: sid})
		if err == nil {
			t.Errorf("expected error for sid %q", sid)
		}
	}
}

// TestMCPToolContract runs the registered MCP tool end-to-end against a
// live MCPServer. Confirms the input-schema → composer → JSON result
// path that rfc003's UserPromptSubmit hook will exercise.
func TestMCPToolContract(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog", Port: 0}
	nucleus := &Nucleus{Name: "test"}
	proc := NewProcess(cfg, nucleus)
	m := NewMCPServer(cfg, nucleus, proc)

	// Wire the session backend so the tool has a real bus + handoffs.
	bus := NewBusSessionManager(root)
	m.SetSessionsBackend(bus, NewSessionRegistry(), NewHandoffRegistry())

	// Seed one activity event for sid "alpha-beta-gamma".
	sid := "alpha-beta-gamma"
	_, err := bus.AppendEvent(ActivityChannelForSid(sid), evtTailerBlock, "claude-code:conv-x", map[string]interface{}{
		"kind":           "assistant",
		"source_channel": "claude-code:conv-x",
		"timestamp":      time.Now().UTC().Format(time.RFC3339Nano),
		"ref":            "ref-mcp",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	budget := 500
	result, _, err := m.toolRenderPeerAwarenessPacket(context.Background(), nil, peerAwarenessInput{
		Sid:    sid,
		Budget: budget,
	})
	if err != nil {
		t.Fatalf("tool err: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatalf("empty result")
	}

	// The MCP surface returns the JSON as TextContent.
	var body PeerAwarenessResult
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if err := json.Unmarshal([]byte(tc.Text), &body); err != nil {
				t.Fatalf("unmarshal: %v; text=%s", err, tc.Text)
			}
			break
		}
	}
	if !strings.Contains(body.Packet, "MY RECENT ACTIVITY") {
		t.Errorf("missing activity section in MCP response:\n%s", body.Packet)
	}
	if body.TokenCount <= 0 {
		t.Errorf("token_count = %d, want >0", body.TokenCount)
	}
	if len(body.Sources) == 0 {
		t.Errorf("expected sources[]; got none")
	}
}

// TestMCPToolContractBadSid: the tool returns an error result (not a
// panic) when the sid is unsafe.
func TestMCPToolContractBadSid(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &Config{WorkspaceRoot: root, CogDir: root + "/.cog", Port: 0}
	nucleus := &Nucleus{Name: "test"}
	proc := NewProcess(cfg, nucleus)
	m := NewMCPServer(cfg, nucleus, proc)

	result, _, err := m.toolRenderPeerAwarenessPacket(context.Background(), nil, peerAwarenessInput{Sid: "../escape"})
	if err != nil {
		t.Fatalf("tool err (should be result.IsError): %v", err)
	}
	if result == nil || !result.IsError {
		t.Errorf("expected IsError=true for bad sid; got %+v", result)
	}
}

// TestNormalizeRequestDefaults checks the budget + window clamping logic.
// Belt-and-braces: HTTP layer also validates, but the composer must
// enforce its own bounds since MCP callers bypass the HTTP parser.
func TestNormalizeRequestDefaults(t *testing.T) {
	t.Parallel()
	r := normalizePeerAwarenessRequest(PeerAwarenessRequest{Sid: "a-b-c"})
	if r.Budget != DefaultPeerAwarenessBudget {
		t.Errorf("Budget default = %d, want %d", r.Budget, DefaultPeerAwarenessBudget)
	}
	if r.Window != DefaultPeerAwarenessWindow {
		t.Errorf("Window default = %v, want %v", r.Window, DefaultPeerAwarenessWindow)
	}
	if r.Now.IsZero() {
		t.Errorf("Now should have been populated")
	}

	r = normalizePeerAwarenessRequest(PeerAwarenessRequest{Sid: "a-b-c", Budget: 99999})
	if r.Budget != MaxPeerAwarenessBudget {
		t.Errorf("Budget not clamped to max: got %d", r.Budget)
	}
}
