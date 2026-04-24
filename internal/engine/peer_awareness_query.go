// peer_awareness_query.go — the READ side of the 4E ambient-awareness loop.
//
// Phase 1B of the session-activity-channel sequence. Phase 1A (commit
// d9737a0 on feat/session-activity-channel) publishes tailer.block events
// onto channel.<sid>.activity whenever a session's harness emits a
// normalized CogBlock. This file composes those events — plus overlapping
// peer activity, open handoffs, and recent coord chatter — into a
// budget-capped human-readable "peer-awareness packet" that a
// UserPromptSubmit hook can prepend to a session's next prompt.
//
// The packet has four sections in priority order:
//
//  1. My recent activity         (channel.<sid>.activity, last window)
//  2. Open handoffs for this sid (bus_handoffs, offers/claims/completes)
//  3. Peer sessions overlapping  (attention-table co-focus + their
//     channel.<other-sid>.activity events)
//  4. Recent coord/impl events   (bus_broadcast, type prefix match)
//
// Anti-echo:
//
//	The spec's "anti-echo" rule says peer attention that *postdates* a
//	prior packet mention of the same target should be discounted — else
//	two peers ping-pong on each other's echoes. MVP: we compute an
//	echo-risk boolean per source based on whether this sid previously
//	emitted a peer_awareness.rendered event (on bus_peer_awareness) that
//	referenced the same target/peer pair. The subtractive rendering is
//	left for a follow-up; this file tags sources and emits the render
//	event so later iterations can close the loop without a schema change.
//
// Token budgeting:
//
//	estTokens (len/4) is used for the conservative approximation. Each
//	section gets a soft cap proportional to the total budget; within a
//	section events are packed FIFO until the section cap is hit. A single
//	event that exceeds its section budget is truncated (but the source
//	reference is always preserved). If nothing fits, packet="" is
//	returned with sources=[] and the caller should still treat that as a
//	success (HTTP 200) — 503 is reserved for dependency outage.
//
// Design notes:
//
//   - Pure function over its inputs (bus manager, attention log, handoff
//     registry). No global state, no process singleton access. This makes
//     it testable in isolation and keeps the HTTP/MCP handlers thin.
//   - DefaultPeerAwarenessBudget / DefaultPeerAwarenessWindow exported so
//     HTTP + MCP surfaces share a single source of truth.
//   - Returns a dedicated PeerAwarenessResult struct; marshal shape is
//     fixed by the contract rfc003's UserPromptSubmit hook already
//     encodes against (packet, token_count, sources[]).
package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ─── contract defaults ───────────────────────────────────────────────────────

const (
	// DefaultPeerAwarenessBudget is the token budget used when the caller
	// doesn't provide one. Keep small — packet is prefixed to every turn.
	DefaultPeerAwarenessBudget = 500

	// DefaultPeerAwarenessWindow is how far back the query looks when the
	// caller doesn't specify. 15 minutes balances freshness with useful
	// multi-peer context when several agents are hopping between turns.
	DefaultPeerAwarenessWindow = 15 * time.Minute

	// MaxPeerAwarenessBudget is a hard upper bound, defensive against
	// clients that pass unreasonably large budgets. 4000 ≈ 16 KB of
	// text, which is well within any reasonable prompt-preamble slot.
	MaxPeerAwarenessBudget = 4000

	// PeerAwarenessRenderBus is the well-known bus that receives the
	// peer_awareness.rendered causality beacon. rfc003 listens here so
	// downstream iterations can compute the anti-echo discount.
	PeerAwarenessRenderBus = "bus_peer_awareness"

	// EvtPeerAwarenessRendered marks a render. Payload carries sid,
	// ts, sources[] (same shape as the response).
	EvtPeerAwarenessRendered = "peer_awareness.rendered"

	// Activity channel naming — mirrors Phase 1A (process.publishSessionActivity).
	activityChannelPrefix = "channel."
	activityChannelSuffix = ".activity"

	// Tailer block event type — Phase 1A contract.
	evtTailerBlock = "tailer.block"

	// Section weights: total budget divides like 0.35 / 0.20 / 0.30 / 0.15.
	// Favour first-person recent activity (what am I doing), then peer
	// overlap (who else is looking here), then handoffs (what's pending),
	// then coord chatter (the loudest firehose, so smallest slice).
	//
	// We renormalize any residual unused budget into later sections so
	// packets that would otherwise underfill stay useful.
	sectionWeightMyActivity  = 0.35
	sectionWeightHandoffs    = 0.20
	sectionWeightPeerOverlap = 0.30
	sectionWeightCoord       = 0.15
)

// sidPattern enforces the activity channel's safe-sid shape so
// channel.<sid>.activity never contains path separators or shell
// metacharacters. Matches the sessionIDPattern from sessions.go but is
// deliberately stricter (no leading/trailing hyphens, no consecutive
// hyphens). Copied rather than reused because peer-awareness may grow to
// accept identity-layer sids that sessions.go doesn't see.
var sidPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,126}[a-z0-9]$`)

// ValidateSid returns an error iff sid is unsafe to use in a bus id path
// or an activity channel name. Empty strings, path separators, consecutive
// hyphens, and anything outside the lowercase-hyphen charset are rejected.
func ValidateSid(sid string) error {
	if sid == "" {
		return fmt.Errorf("sid is required")
	}
	if len(sid) > 128 {
		return fmt.Errorf("sid too long (max 128 chars)")
	}
	if strings.ContainsAny(sid, "/\\ \t\n\r\x00") {
		return fmt.Errorf("sid contains forbidden characters")
	}
	if strings.Contains(sid, "--") {
		return fmt.Errorf("sid must not contain consecutive hyphens")
	}
	if !sidPattern.MatchString(sid) {
		return fmt.Errorf("sid must be lowercase hex/alphanum with inner hyphens only")
	}
	return nil
}

// ActivityChannelForSid returns the Phase 1A channel name for a sid.
// Precondition: sid must have already passed ValidateSid. The helper is
// intentionally simple so the hash-chain locations of these buses stay
// grep-able from both the write and read sides.
func ActivityChannelForSid(sid string) string {
	return activityChannelPrefix + sid + activityChannelSuffix
}

// ─── request / response types ────────────────────────────────────────────────

// PeerAwarenessRequest is the normalized input to the query. The HTTP
// handler + MCP tool both construct this before calling
// RenderPeerAwarenessPacket, which keeps validation + clamping in one place.
type PeerAwarenessRequest struct {
	// Sid is the session whose perspective the packet is rendered from.
	// Must pass ValidateSid — callers should validate earlier and surface
	// a 400 on failure; RenderPeerAwarenessPacket re-validates defensively.
	Sid string

	// Budget is the token budget. Zero or negative → DefaultPeerAwarenessBudget.
	// Clamped to [1, MaxPeerAwarenessBudget].
	Budget int

	// Window is how far back to pull events. Zero → DefaultPeerAwarenessWindow.
	Window time.Duration

	// IncludePeers gates section 3 (peer-overlap). Defaults to true at
	// the HTTP/MCP layer; a zero-value bool on a freshly-built request
	// means "not yet decided" — callers set this explicitly.
	IncludePeers bool

	// Now is a clock injection point for deterministic tests. Zero →
	// time.Now().UTC().
	Now time.Time
}

// PeerAwarenessSource references one underlying event that contributed to
// the rendered packet. Callers can dereference `Ref` (or the bus+seq
// pointed to by Type/Sid) to audit why a line showed up.
type PeerAwarenessSource struct {
	Sid      string `json:"sid,omitempty"`
	Type     string `json:"type"`
	Ts       string `json:"ts"`
	Ref      string `json:"ref,omitempty"`
	EchoRisk bool   `json:"echo_risk,omitempty"`
	BusID    string `json:"bus_id,omitempty"`
	Seq      int    `json:"seq,omitempty"`
	Section  string `json:"section,omitempty"`
}

// PeerAwarenessResult is the response shape shared by the HTTP handler
// and MCP tool. Field names match the contract rfc003's hook is encoded
// against — do not rename without a coordinated update on both sides.
type PeerAwarenessResult struct {
	Packet     string                `json:"packet"`
	TokenCount int                   `json:"token_count"`
	Sources    []PeerAwarenessSource `json:"sources"`

	// Notes surfaces MVP caveats — currently "anti_echo_mvp" meaning the
	// tagging-only implementation is active (full subtractive filter is
	// a follow-up). Empty when no notes apply.
	Notes []string `json:"notes,omitempty"`
}

// ─── dependencies (interfaces for test injection) ────────────────────────────

// peerAwarenessDeps bundles the four data sources the composer reads. An
// interface bundle keeps the unit tests hermetic: each dep has a minimal
// method set, so a test can provide a fake without implementing the full
// BusSessionManager / HandoffRegistry APIs.
type peerAwarenessDeps struct {
	bus      peerAwarenessBusReader
	attn     peerAwarenessAttentionReader
	handoffs peerAwarenessHandoffReader
	renderer peerAwarenessRenderEmitter // nil is OK — emission is best-effort
}

// peerAwarenessBusReader is the subset of BusSessionManager the composer
// needs. ReadEvents returns a full bus backlog; callers filter down.
type peerAwarenessBusReader interface {
	ReadEvents(busID string) ([]BusBlock, error)
}

// peerAwarenessAttentionReader is the subset of the attention log the
// composer needs. Returning recent signals avoids streaming the entire
// on-disk log into memory.
type peerAwarenessAttentionReader interface {
	RecentSignals(n int) []PeerAwarenessAttentionSignal
}

// PeerAwarenessAttentionSignal is a minimal copy of the attentionSignal
// struct from serve_attention.go, exported so fakes + HTTP fixtures can
// construct one without importing the private type. The fields that
// matter for peer-overlap computation are ParticipantID, TargetURI, and
// OccurredAt.
type PeerAwarenessAttentionSignal struct {
	ParticipantID string
	TargetURI     string
	SignalType    string
	OccurredAt    string
}

// peerAwarenessHandoffReader returns handoff rows. The composer treats
// every row uniformly (open, claimed, completed) and filters by time +
// sid relationship in-place.
type peerAwarenessHandoffReader interface {
	Snapshot() []*HandoffState
}

// peerAwarenessRenderEmitter is the optional side-channel that
// appends a peer_awareness.rendered event so the anti-echo feedback loop
// can observe which sources this render referenced. Nil is fine — the
// packet is still returned; callers just lose the causality trail.
type peerAwarenessRenderEmitter interface {
	AppendEvent(busID, eventType, from string, payload map[string]interface{}) (*BusBlock, error)
}

// ─── adapters for real kernel types ──────────────────────────────────────────

// attentionLogAdapter wraps *attentionLog so it satisfies the minimal
// peerAwarenessAttentionReader contract. The internal attentionSignal type
// is unexported; we project onto PeerAwarenessAttentionSignal here.
type attentionLogAdapter struct{ log *attentionLog }

func (a attentionLogAdapter) RecentSignals(n int) []PeerAwarenessAttentionSignal {
	if a.log == nil {
		return nil
	}
	raw := a.log.recentSignals(n)
	out := make([]PeerAwarenessAttentionSignal, len(raw))
	for i, s := range raw {
		out[i] = PeerAwarenessAttentionSignal{
			ParticipantID: s.ParticipantID,
			TargetURI:     s.TargetURI,
			SignalType:    s.SignalType,
			OccurredAt:    s.OccurredAt,
		}
	}
	return out
}

// fileAttentionReader reads attention.jsonl directly — used when the
// server wasn't constructed with a shared attentionLog (e.g. test
// harnesses that seed the file but skip NewServer). Returned when the
// workspace has a .cog/run/attention.jsonl but no in-memory log handle.
type fileAttentionReader struct{ path string }

func (r fileAttentionReader) RecentSignals(n int) []PeerAwarenessAttentionSignal {
	f, err := os.Open(r.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []PeerAwarenessAttentionSignal
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var raw struct {
			ParticipantID string `json:"participant_id"`
			TargetURI     string `json:"target_uri"`
			SignalType    string `json:"signal_type"`
			OccurredAt    string `json:"occurred_at"`
		}
		if jsonUnmarshalBytes(sc.Bytes(), &raw) == nil {
			all = append(all, PeerAwarenessAttentionSignal{
				ParticipantID: raw.ParticipantID,
				TargetURI:     raw.TargetURI,
				SignalType:    raw.SignalType,
				OccurredAt:    raw.OccurredAt,
			})
		}
	}
	if len(all) <= n {
		return all
	}
	return all[len(all)-n:]
}

// jsonUnmarshalBytes is a thin wrapper so the file reader can live in this
// file without leaking the encoding/json dependency into every call site.
// Kept private to make swapping in a streaming / trace-aware reader easy.
func jsonUnmarshalBytes(data []byte, out interface{}) error {
	return json.Unmarshal(data, out)
}

// ─── main entry point ────────────────────────────────────────────────────────

// RenderPeerAwarenessPacket composes the packet, returns its sources, and
// best-effort emits the peer_awareness.rendered beacon. Never panics —
// individual section failures degrade to an empty section with a single
// diagnostic source line so the packet remains useful.
func RenderPeerAwarenessPacket(deps peerAwarenessDeps, req PeerAwarenessRequest) (*PeerAwarenessResult, error) {
	if err := ValidateSid(req.Sid); err != nil {
		return nil, err
	}
	req = normalizePeerAwarenessRequest(req)

	// Compute section budgets. Token budget is shared across all sections
	// with an explicit weighting — favour first-person activity because
	// the downstream hook prepends the packet to its own prompt.
	totalBudget := req.Budget
	sectionCaps := map[string]int{
		"my_activity":   clampNonNeg(int(float64(totalBudget) * sectionWeightMyActivity)),
		"handoffs":      clampNonNeg(int(float64(totalBudget) * sectionWeightHandoffs)),
		"peer_activity": clampNonNeg(int(float64(totalBudget) * sectionWeightPeerOverlap)),
		"coord":         clampNonNeg(int(float64(totalBudget) * sectionWeightCoord)),
	}

	var (
		sections []packetSection
		sources  []PeerAwarenessSource
	)

	// Section 1 — My recent activity.
	mySec, mySrc := composeMyActivity(deps.bus, req, sectionCaps["my_activity"])
	sections = append(sections, mySec)
	sources = append(sources, mySrc...)

	// Section 2 — Open handoffs relevant to this sid.
	hoSec, hoSrc := composeHandoffs(deps.handoffs, req, sectionCaps["handoffs"])
	sections = append(sections, hoSec)
	sources = append(sources, hoSrc...)

	// Section 3 — Peer sessions with attention overlap.
	if req.IncludePeers {
		peerSec, peerSrc := composePeerOverlap(deps.bus, deps.attn, req, sectionCaps["peer_activity"])
		sections = append(sections, peerSec)
		sources = append(sources, peerSrc...)
	}

	// Section 4 — Recent coord/impl events from bus_broadcast.
	coordSec, coordSrc := composeCoord(deps.bus, req, sectionCaps["coord"])
	sections = append(sections, coordSec)
	sources = append(sources, coordSrc...)

	// Assemble the packet. Empty sections are omitted entirely (no header,
	// no blank lines) to keep the packet compact under tight budgets.
	packet := renderSections(sections)
	tokenCount := estTokens(packet)

	// Apply the final hard cap — sections are already budget-sized but
	// rounding could push us one or two over. Never exceed the caller's
	// requested budget.
	if tokenCount > totalBudget {
		packet, sources = truncatePacketToBudget(packet, sources, totalBudget)
		tokenCount = estTokens(packet)
	}

	res := &PeerAwarenessResult{
		Packet:     packet,
		TokenCount: tokenCount,
		Sources:    sources,
		Notes:      []string{"anti_echo_mvp"},
	}

	// Best-effort beacon for the anti-echo feedback loop. Failure is
	// logged elsewhere and intentionally not surfaced — the render was
	// still valid even if the bus append failed.
	emitPeerAwarenessRendered(deps.renderer, req, sources)

	return res, nil
}

// normalizePeerAwarenessRequest clamps budget + window to sane bounds
// and fills in defaults. Callers should still validate sid explicitly
// before arriving here — re-validation is defensive.
func normalizePeerAwarenessRequest(req PeerAwarenessRequest) PeerAwarenessRequest {
	if req.Budget <= 0 {
		req.Budget = DefaultPeerAwarenessBudget
	}
	if req.Budget > MaxPeerAwarenessBudget {
		req.Budget = MaxPeerAwarenessBudget
	}
	if req.Window <= 0 {
		req.Window = DefaultPeerAwarenessWindow
	}
	if req.Now.IsZero() {
		req.Now = time.Now().UTC()
	} else {
		req.Now = req.Now.UTC()
	}
	return req
}

func clampNonNeg(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// ─── section 1: my activity ──────────────────────────────────────────────────

// composeMyActivity renders the last window's tailer.block events from
// channel.<sid>.activity. One line per event, sorted chronologically.
// Events older than window are dropped. If no publisher has fed the
// channel (e.g. Phase 1A not landed yet) the section is empty — that's
// expected, not an error.
func composeMyActivity(bus peerAwarenessBusReader, req PeerAwarenessRequest, capTokens int) (packetSection, []PeerAwarenessSource) {
	sec := packetSection{heading: "MY RECENT ACTIVITY"}
	if bus == nil || capTokens <= 0 {
		return sec, nil
	}
	busID := ActivityChannelForSid(req.Sid)
	events, err := bus.ReadEvents(busID)
	if err != nil || len(events) == 0 {
		return sec, nil
	}

	sort.SliceStable(events, func(i, j int) bool { return events[i].Ts < events[j].Ts })

	cutoff := req.Now.Add(-req.Window)
	var lines []string
	var sources []PeerAwarenessSource
	used := 0

	for _, e := range events {
		if e.Type != evtTailerBlock {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, e.Ts)
		if err != nil || ts.Before(cutoff) {
			continue
		}
		kind, _ := e.Payload["kind"].(string)
		src, _ := e.Payload["source_channel"].(string)
		ref, _ := e.Payload["ref"].(string)
		line := fmt.Sprintf("  %s %s: %s", formatShortTs(ts), safeField(kind), safeField(src))
		line = truncateLine(line, capTokens-used)
		if line == "" {
			break
		}
		lines = append(lines, line)
		used += estTokens(line + "\n")
		sources = append(sources, PeerAwarenessSource{
			Sid:     req.Sid,
			Type:    e.Type,
			Ts:      e.Ts,
			Ref:     ref,
			BusID:   busID,
			Seq:     e.Seq,
			Section: "my_activity",
		})
		if used >= capTokens {
			break
		}
	}
	sec.lines = lines
	return sec, sources
}

// ─── section 2: handoffs ─────────────────────────────────────────────────────

// composeHandoffs filters the handoff registry for rows where this sid is
// the offeree, claimant, or the session that ended the handoff. Each
// matching row contributes one line summarising state + counterparty.
func composeHandoffs(handoffs peerAwarenessHandoffReader, req PeerAwarenessRequest, capTokens int) (packetSection, []PeerAwarenessSource) {
	sec := packetSection{heading: "OPEN HANDOFFS"}
	if handoffs == nil || capTokens <= 0 {
		return sec, nil
	}
	rows := handoffs.Snapshot()
	if len(rows) == 0 {
		return sec, nil
	}

	cutoff := req.Now.Add(-req.Window)
	relevant := make([]*HandoffState, 0, len(rows))
	for _, h := range rows {
		if h == nil {
			continue
		}
		if !handoffRelevantToSid(h, req.Sid) {
			continue
		}
		// Surface anything created, claimed, or completed inside the window.
		if handoffTouchedWithin(h, cutoff) {
			relevant = append(relevant, h)
		}
	}
	// Newest-first so the most recent activity wins the budget.
	sort.SliceStable(relevant, func(i, j int) bool {
		return handoffLastTouched(relevant[i]).After(handoffLastTouched(relevant[j]))
	})

	var lines []string
	var sources []PeerAwarenessSource
	used := 0

	for _, h := range relevant {
		line := fmt.Sprintf("  %s [%s] %s → %s  %s",
			formatShortTs(handoffLastTouched(h)),
			safeField(h.State),
			safeField(h.FromSession),
			safeField(firstNonEmpty(h.ToSession, h.ClaimingSession, "(open)")),
			safeField(h.Reason))
		line = truncateLine(line, capTokens-used)
		if line == "" {
			break
		}
		lines = append(lines, line)
		used += estTokens(line + "\n")
		sources = append(sources, PeerAwarenessSource{
			Sid:     req.Sid,
			Type:    "handoff." + h.State,
			Ts:      handoffLastTouched(h).Format(time.RFC3339Nano),
			Ref:     h.HandoffID,
			BusID:   BusHandoffs,
			Section: "handoffs",
		})
		if used >= capTokens {
			break
		}
	}
	sec.lines = lines
	return sec, sources
}

// handoffRelevantToSid returns true when sid participates in the handoff
// in any role we care about: source, declared target, claimant, or the
// completing session.
func handoffRelevantToSid(h *HandoffState, sid string) bool {
	if h == nil || sid == "" {
		return false
	}
	if h.FromSession == sid || h.ToSession == sid ||
		h.ClaimingSession == sid || h.CompletingSession == sid {
		return true
	}
	return false
}

// handoffTouchedWithin is true when any of the handoff's state-transition
// timestamps falls on or after cutoff. Lets completed handoffs still show
// up if they wrapped inside the window.
func handoffTouchedWithin(h *HandoffState, cutoff time.Time) bool {
	last := handoffLastTouched(h)
	return !last.IsZero() && !last.Before(cutoff)
}

func handoffLastTouched(h *HandoffState) time.Time {
	if h == nil {
		return time.Time{}
	}
	candidates := []time.Time{h.CompletedAt, h.ClaimedAt, h.CreatedAt}
	var best time.Time
	for _, t := range candidates {
		if t.After(best) {
			best = t
		}
	}
	return best
}

// ─── section 3: peer overlap ─────────────────────────────────────────────────

// composePeerOverlap is the heart of the 4E loop — it finds other sids
// whose recent attention targets intersect this sid's, then pulls their
// most recent activity events so the packet can say "peer P is also
// looking at target X".
//
// Anti-echo: every source flagged with echo_risk=true here means the
// receiving hook should consider discounting subtractively. MVP leaves
// the rendering itself intact and only tags — the subtractive path is a
// follow-up iteration once we have enough data to tune it.
func composePeerOverlap(bus peerAwarenessBusReader, attn peerAwarenessAttentionReader, req PeerAwarenessRequest, capTokens int) (packetSection, []PeerAwarenessSource) {
	sec := packetSection{heading: "PEER OVERLAP"}
	if bus == nil || attn == nil || capTokens <= 0 {
		return sec, nil
	}
	recent := attn.RecentSignals(500)
	if len(recent) == 0 {
		return sec, nil
	}
	cutoff := req.Now.Add(-req.Window)

	// Step 1: which targets has *this* sid recently attended to?
	myTargets := map[string]bool{}
	for _, s := range recent {
		if s.ParticipantID != req.Sid {
			continue
		}
		if !signalWithin(s, cutoff) {
			continue
		}
		myTargets[s.TargetURI] = true
	}
	if len(myTargets) == 0 {
		return sec, nil
	}

	// Step 2: which *other* participants have recent attention on any of
	// those targets? De-dup per peer-sid.
	peerTargets := map[string]map[string]bool{}
	for _, s := range recent {
		if s.ParticipantID == "" || s.ParticipantID == req.Sid {
			continue
		}
		if !myTargets[s.TargetURI] {
			continue
		}
		if !signalWithin(s, cutoff) {
			continue
		}
		if peerTargets[s.ParticipantID] == nil {
			peerTargets[s.ParticipantID] = map[string]bool{}
		}
		peerTargets[s.ParticipantID][s.TargetURI] = true
	}
	if len(peerTargets) == 0 {
		return sec, nil
	}

	// Stable iteration order makes the rendered packet deterministic.
	peerSids := make([]string, 0, len(peerTargets))
	for p := range peerTargets {
		peerSids = append(peerSids, p)
	}
	sort.Strings(peerSids)

	// Echo-risk set: peers this sid has previously rendered a packet for,
	// computed from the render-bus backlog. MVP tags only.
	echoPeers := echoRiskPeersForSid(bus, req.Sid)

	var lines []string
	var sources []PeerAwarenessSource
	used := 0

	for _, p := range peerSids {
		// Guard: if the peer name isn't a valid sid we can't read their
		// activity bus. Still list them — the overlap remains useful.
		shortTargets := shortTargetSummary(peerTargets[p], 3)
		peerLine := fmt.Sprintf("  peer %s  overlap: %s", safeField(p), shortTargets)
		peerLine = truncateLine(peerLine, capTokens-used)
		if peerLine == "" {
			break
		}
		lines = append(lines, peerLine)
		used += estTokens(peerLine + "\n")

		// Try to pull a peer activity event.
		var peerRef string
		var peerSeq int
		var peerTs string
		if ValidateSid(p) == nil {
			peerBusID := ActivityChannelForSid(p)
			peerEvents, err := bus.ReadEvents(peerBusID)
			if err == nil && len(peerEvents) > 0 {
				sort.SliceStable(peerEvents, func(i, j int) bool { return peerEvents[i].Ts > peerEvents[j].Ts })
				for _, e := range peerEvents {
					if e.Type != evtTailerBlock {
						continue
					}
					ts, err := time.Parse(time.RFC3339Nano, e.Ts)
					if err != nil || ts.Before(cutoff) {
						continue
					}
					kind, _ := e.Payload["kind"].(string)
					src, _ := e.Payload["source_channel"].(string)
					peerRef, _ = e.Payload["ref"].(string)
					peerSeq = e.Seq
					peerTs = e.Ts
					activityLine := fmt.Sprintf("    %s %s: %s",
						formatShortTs(ts), safeField(kind), safeField(src))
					activityLine = truncateLine(activityLine, capTokens-used)
					if activityLine == "" {
						break
					}
					lines = append(lines, activityLine)
					used += estTokens(activityLine + "\n")
					break
				}
			}
		}

		sources = append(sources, PeerAwarenessSource{
			Sid:      p,
			Type:     evtTailerBlock,
			Ts:       peerTs,
			Ref:      peerRef,
			EchoRisk: echoPeers[p],
			BusID:    ActivityChannelForSid(p),
			Seq:      peerSeq,
			Section:  "peer_activity",
		})
		if used >= capTokens {
			break
		}
	}
	sec.lines = lines
	return sec, sources
}

// signalWithin returns true when the signal's occurred_at is inside the window.
// Unparseable timestamps are conservatively treated as in-window (better to
// include than to silently drop).
func signalWithin(s PeerAwarenessAttentionSignal, cutoff time.Time) bool {
	if s.OccurredAt == "" {
		return true
	}
	ts, err := time.Parse(time.RFC3339, s.OccurredAt)
	if err != nil {
		// Try RFC3339 with nanos too.
		if t2, err2 := time.Parse(time.RFC3339Nano, s.OccurredAt); err2 == nil {
			ts = t2
		} else {
			return true
		}
	}
	return !ts.Before(cutoff)
}

// echoRiskPeersForSid returns the set of peer sids that this sid has
// previously rendered a peer-awareness packet about. Used to flag
// echo_risk sources in section 3. Never returns nil — on any error,
// returns an empty map (so all peers get echo_risk=false).
func echoRiskPeersForSid(bus peerAwarenessBusReader, sid string) map[string]bool {
	out := map[string]bool{}
	if bus == nil {
		return out
	}
	events, err := bus.ReadEvents(PeerAwarenessRenderBus)
	if err != nil || len(events) == 0 {
		return out
	}
	for _, e := range events {
		if e.Type != EvtPeerAwarenessRendered {
			continue
		}
		recvSid, _ := e.Payload["sid"].(string)
		if recvSid != sid {
			continue
		}
		// The rendered beacon carries a `peers[]` list of sids it mentioned.
		peers, _ := e.Payload["peers"].([]interface{})
		for _, p := range peers {
			if s, ok := p.(string); ok && s != "" {
				out[s] = true
			}
		}
	}
	return out
}

// shortTargetSummary formats up to max targets as a comma-separated list,
// collapsing the rest into "+N more".
func shortTargetSummary(targets map[string]bool, max int) string {
	if len(targets) == 0 {
		return ""
	}
	keys := make([]string, 0, len(targets))
	for k := range targets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) <= max {
		return strings.Join(keys, ", ")
	}
	head := keys[:max]
	return strings.Join(head, ", ") + fmt.Sprintf(" (+%d more)", len(keys)-max)
}

// ─── section 4: coord/impl chatter ───────────────────────────────────────────

// composeCoord pulls bus_broadcast events whose type prefix is coord.* or
// impl.* and occurred inside the window. This is the lowest-priority
// section because bus_broadcast is globally noisy; we keep the budget
// small and the output one-line-per-event.
func composeCoord(bus peerAwarenessBusReader, req PeerAwarenessRequest, capTokens int) (packetSection, []PeerAwarenessSource) {
	sec := packetSection{heading: "COORD CHATTER"}
	if bus == nil || capTokens <= 0 {
		return sec, nil
	}
	events, err := bus.ReadEvents(BusBroadcast)
	if err != nil || len(events) == 0 {
		return sec, nil
	}
	cutoff := req.Now.Add(-req.Window)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Ts > events[j].Ts })

	var lines []string
	var sources []PeerAwarenessSource
	used := 0

	for _, e := range events {
		if !(strings.HasPrefix(e.Type, "coord.") || strings.HasPrefix(e.Type, "impl.")) {
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, e.Ts)
		if err != nil || ts.Before(cutoff) {
			continue
		}
		summary := firstNonEmpty(
			stringFromPayload(e.Payload, "summary"),
			stringFromPayload(e.Payload, "message"),
			stringFromPayload(e.Payload, "content"),
			"",
		)
		line := fmt.Sprintf("  %s %s from %s: %s",
			formatShortTs(ts), safeField(e.Type), safeField(e.From), safeField(summary))
		line = truncateLine(line, capTokens-used)
		if line == "" {
			break
		}
		lines = append(lines, line)
		used += estTokens(line + "\n")
		sources = append(sources, PeerAwarenessSource{
			Type:    e.Type,
			Ts:      e.Ts,
			BusID:   BusBroadcast,
			Seq:     e.Seq,
			Section: "coord",
		})
		if used >= capTokens {
			break
		}
	}
	sec.lines = lines
	return sec, sources
}

func stringFromPayload(p map[string]interface{}, key string) string {
	if p == nil {
		return ""
	}
	v, ok := p[key].(string)
	if !ok {
		return ""
	}
	return v
}

// ─── rendering helpers ───────────────────────────────────────────────────────

type packetSection struct {
	heading string
	lines   []string
}

// renderSections assembles the packet from populated sections. Empty
// sections are skipped entirely so a packet with only my-activity isn't
// padded with three blank headings.
func renderSections(sections []packetSection) string {
	var parts []string
	for _, sec := range sections {
		if len(sec.lines) == 0 {
			continue
		}
		parts = append(parts, sec.heading+":")
		parts = append(parts, sec.lines...)
	}
	return strings.Join(parts, "\n")
}

// truncatePacketToBudget is a defensive last-mile cap. Sections are
// already sized to the section budget; this only fires when rounding
// pushes the total one or two tokens over. Truncates trailing lines
// (coarsely) until the packet fits, then drops matching sources whose
// rendered line vanished. Source order is preserved; we drop from the
// tail since that's where the lowest-priority content lives.
func truncatePacketToBudget(packet string, sources []PeerAwarenessSource, budget int) (string, []PeerAwarenessSource) {
	if estTokens(packet) <= budget {
		return packet, sources
	}
	// Drop sources (and therefore lines) from the end until we fit.
	// Recompute the packet text each pop so we stay honest about the
	// actual budget — safer than estimating deltas.
	lines := strings.Split(packet, "\n")
	for estTokens(strings.Join(lines, "\n")) > budget && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	packet = strings.Join(lines, "\n")
	if len(sources) > 0 {
		drop := len(sources) / 4
		if drop < 1 {
			drop = 1
		}
		if drop > len(sources) {
			drop = len(sources)
		}
		sources = sources[:len(sources)-drop]
	}
	return packet, sources
}

// formatShortTs renders HH:MM for brevity — timestamps are still carried
// authoritatively in the sources[] list for audit.
func formatShortTs(t time.Time) string {
	if t.IsZero() {
		return "     "
	}
	return t.UTC().Format("15:04")
}

// safeField turns empty strings into a placeholder and strips newlines
// so a multiline payload value can't corrupt the single-line-per-event
// packet layout. Deliberately does NOT escape other control characters —
// the packet is rendered as text into a prompt, where unusual codepoints
// are the author's responsibility.
func safeField(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(empty)"
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// truncateLine enforces the per-line budget. If the line is longer than
// the remaining budget allows, it's truncated with a trailing "…" marker.
// If the budget is already exhausted we return empty so the caller breaks.
func truncateLine(line string, remainingTokens int) string {
	if remainingTokens <= 0 {
		return ""
	}
	lineTokens := estTokens(line + "\n")
	if lineTokens <= remainingTokens {
		return line
	}
	// Truncate from the right, leaving room for an ellipsis. estTokens is
	// len/4, so four chars ≈ one token.
	maxChars := remainingTokens * 4
	if maxChars < 3 {
		// Not enough room to keep any meaningful content.
		return ""
	}
	if maxChars >= len(line) {
		return line
	}
	return line[:maxChars-1] + "…"
}

// ─── render beacon emission ──────────────────────────────────────────────────

// emitPeerAwarenessRendered writes the causality beacon onto
// bus_peer_awareness. Failure is swallowed — the packet is still valid
// even if the beacon can't be written. The payload shape:
//
//	{"sid": "<sid>", "ts": "<rfc3339nano>", "peers": [...],
//	 "sources": [{sid, type, ts, ref}, ...]}
//
// Downstream iterations can read this to compute the anti-echo discount
// without widening the request schema.
func emitPeerAwarenessRendered(emitter peerAwarenessRenderEmitter, req PeerAwarenessRequest, sources []PeerAwarenessSource) {
	if emitter == nil {
		return
	}
	peerSet := map[string]bool{}
	for _, s := range sources {
		if s.Section == "peer_activity" && s.Sid != "" && s.Sid != req.Sid {
			peerSet[s.Sid] = true
		}
	}
	peers := make([]string, 0, len(peerSet))
	for p := range peerSet {
		peers = append(peers, p)
	}
	sort.Strings(peers)
	miniSources := make([]map[string]interface{}, 0, len(sources))
	for _, s := range sources {
		miniSources = append(miniSources, map[string]interface{}{
			"sid":  s.Sid,
			"type": s.Type,
			"ts":   s.Ts,
			"ref":  s.Ref,
		})
	}
	_, _ = emitter.AppendEvent(PeerAwarenessRenderBus, EvtPeerAwarenessRendered, req.Sid, map[string]interface{}{
		"sid":     req.Sid,
		"ts":      req.Now.Format(time.RFC3339Nano),
		"peers":   peers,
		"sources": miniSources,
	})
}

// ─── convenience constructor for HTTP/MCP handlers ───────────────────────────

// NewPeerAwarenessDepsFromServer assembles the default deps bundle from a
// Server's live dependencies. Nil bus or handoff registry yield no-op
// readers so the composer degrades gracefully — the HTTP handler still
// returns 200 with an empty packet when Phase 1A isn't wired or the
// workspace has never seen a handoff.
func NewPeerAwarenessDepsFromServer(s *Server) peerAwarenessDeps {
	deps := peerAwarenessDeps{}
	if s == nil {
		return deps
	}
	if s.busSessions != nil {
		deps.bus = s.busSessions
		deps.renderer = s.busSessions
	}
	if s.attentionLog != nil {
		deps.attn = attentionLogAdapter{log: s.attentionLog}
	} else {
		// Fall back to reading the on-disk file if one exists — tests
		// occasionally seed this without calling NewServer.
		path := filepath.Join(s.cfg.WorkspaceRoot, ".cog", "run", "attention.jsonl")
		if _, err := os.Stat(path); err == nil {
			deps.attn = fileAttentionReader{path: path}
		}
	}
	if s.handoffRegistry != nil {
		deps.handoffs = s.handoffRegistry
	}
	return deps
}
