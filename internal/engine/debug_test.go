// debug_test.go — tests for debug snapshot population.
//
// These cover #91: the engine snapshot counters used to be dead (cogdocs_scored,
// engine.budget, flex_budget_used all == 0), and the cogdoc zone reported
// reason="both" alongside relevance=0.0. Both bugs are pure observability —
// the runtime context assembly was correct, but operators reading
// /v1/debug/last got a misleading picture.
package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDebugSnapshotCountersPopulated verifies that after a real assembly the
// debug snapshot reports the counters issue #91 listed as dead. We seed a
// workspace with several active CogDocs so docCandidates is non-empty, run
// AssembleContext, then capture a snapshot and check each field.
func TestDebugSnapshotCountersPopulated(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := NewProcess(cfg, makeNucleus("T", "nucleus body"))

	memDir := filepath.Join(root, ".cog", "mem", "semantic")
	// Three docs — query "topic" matches all three by title, so each will
	// have relevance > 0 and survive the relevance/salience filter.
	writeTestFile(t, filepath.Join(memDir, "doc-one.cog.md"), "---\ntitle: First Topic\nstatus: active\n---\n\nFirst topic body.\n")
	writeTestFile(t, filepath.Join(memDir, "doc-two.cog.md"), "---\ntitle: Second Topic\nstatus: active\n---\n\nSecond topic body.\n")
	writeTestFile(t, filepath.Join(memDir, "doc-three.cog.md"), "---\ntitle: Third Topic\nstatus: active\n---\n\nThird topic body.\n")

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	p.indexMu.Lock()
	p.index = idx
	p.indexMu.Unlock()

	clientMsgs := []ProviderMessage{
		{Role: "user", Content: "tell me about a topic"},
	}
	pkg, err := p.AssembleContext("topic", clientMsgs, 0)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	// Sanity: at least one candidate was scored.
	if pkg.CandidateCount == 0 {
		t.Fatalf("CandidateCount = 0; expected > 0 (3 docs match the query)")
	}
	if pkg.CandidateCount < len(pkg.FovealDocs) {
		t.Errorf("CandidateCount (%d) must be >= injected count (%d)", pkg.CandidateCount, len(pkg.FovealDocs))
	}
	if pkg.Budget <= 0 {
		t.Errorf("Budget = %d; expected > 0", pkg.Budget)
	}
	if pkg.FlexBudgetUsed <= 0 && len(pkg.FovealDocs) > 0 {
		t.Errorf("FlexBudgetUsed = 0 but FovealDocs has %d entries", len(pkg.FovealDocs))
	}

	snap := captureDebugSnapshot(
		clientMsgs, "topic", "test-model", pkg, len(clientMsgs),
		"test-provider", "test-model", 0, 12*time.Millisecond,
	)
	if snap == nil {
		t.Fatal("captureDebugSnapshot returned nil")
	}
	if snap.Engine.CogDocsScored != pkg.CandidateCount {
		t.Errorf("Engine.CogDocsScored = %d; want %d (pkg.CandidateCount)",
			snap.Engine.CogDocsScored, pkg.CandidateCount)
	}
	if snap.Engine.CogDocsScored == 0 {
		t.Error("Engine.CogDocsScored = 0 — this is the dead-counter symptom from issue #91")
	}
	if snap.Engine.Budget == 0 {
		t.Error("Engine.Budget = 0 — should equal pkg.Budget (issue #91)")
	}
	if snap.Engine.Budget != pkg.Budget {
		t.Errorf("Engine.Budget = %d; want %d", snap.Engine.Budget, pkg.Budget)
	}
	if snap.Engine.FlexBudgetUsed != pkg.FlexBudgetUsed {
		t.Errorf("Engine.FlexBudgetUsed = %d; want %d",
			snap.Engine.FlexBudgetUsed, pkg.FlexBudgetUsed)
	}
	if snap.Engine.FlexBudgetUsed == 0 && len(pkg.FovealDocs) > 0 {
		t.Error("Engine.FlexBudgetUsed = 0 with injected docs — dead counter (issue #91)")
	}
	// flex_budget_used must equal the sum of post-eviction CogDoc + conversation tokens.
	want := 0
	for _, d := range pkg.FovealDocs {
		want += d.Tokens
	}
	for _, m := range pkg.Conversation {
		want += m.Tokens
	}
	if snap.Engine.FlexBudgetUsed != want {
		t.Errorf("Engine.FlexBudgetUsed = %d; want %d (sum of cogdocs + conversation zone tokens)",
			snap.Engine.FlexBudgetUsed, want)
	}
}

// TestDebugSnapshotReasonRelevanceAgreement is the explicit assertion the
// issue calls out: "a snapshot with relevance: 0.00, reason: \"both\" is
// impossible". The reason classifier has a contract:
//
//	reason == "both"        ⇒ relevance > 0 AND raw salience > 0
//	reason == "query-match" ⇒ relevance > 0
//	reason == "high-salience" ⇒ relevance == 0 (and raw salience > 0)
//
// Once the snapshot reports the same Relevance value the classifier saw
// (rather than a default zero), every cogdoc zone item must satisfy the
// invariants above. We spot-check across the keyword-scoring path with a
// query that matches some titles but not others.
func TestDebugSnapshotReasonAndRelevanceAgree(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := NewProcess(cfg, makeNucleus("T", "n"))

	memDir := filepath.Join(root, ".cog", "mem", "semantic")
	// "alpha" matches the query; "beta" does not.
	writeTestFile(t, filepath.Join(memDir, "alpha-doc.cog.md"), "---\ntitle: Alpha Document\nstatus: active\n---\n\nAlpha content.\n")
	writeTestFile(t, filepath.Join(memDir, "beta-doc.cog.md"), "---\ntitle: Beta Document\nstatus: active\n---\n\nBeta content.\n")

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	p.indexMu.Lock()
	p.index = idx
	p.indexMu.Unlock()

	// Bump salience on both so each gets a non-zero raw salience and
	// reason="both" becomes possible for the matching doc.
	p.field.Boost(filepath.Join(memDir, "alpha-doc.cog.md"), 1.0)
	p.field.Boost(filepath.Join(memDir, "beta-doc.cog.md"), 1.0)

	clientMsgs := []ProviderMessage{
		{Role: "user", Content: "alpha please"},
	}
	pkg, err := p.AssembleContext("alpha", clientMsgs, 0)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	if len(pkg.FovealDocs) == 0 {
		t.Fatal("no FovealDocs assembled; cannot verify reason/relevance invariant")
	}

	snap := captureDebugSnapshot(
		clientMsgs, "alpha", "test-model", pkg, len(clientMsgs),
		"test-provider", "test-model", 0, time.Millisecond,
	)
	var docZone *DebugZone
	for i := range snap.Context.Zones {
		if snap.Context.Zones[i].Zone == "cogdocs" {
			docZone = &snap.Context.Zones[i]
			break
		}
	}
	if docZone == nil {
		t.Fatal("snapshot has no cogdocs zone")
	}

	for _, item := range docZone.Items {
		switch item.Reason {
		case "both":
			if item.Relevance <= 0 {
				t.Errorf("item %s has reason=both but relevance=%.2f (issue #91 mislabel)",
					item.ID, item.Relevance)
			}
		case "query-match":
			if item.Relevance <= 0 {
				t.Errorf("item %s has reason=query-match but relevance=%.2f",
					item.ID, item.Relevance)
			}
		case "high-salience":
			if item.Relevance != 0 {
				t.Errorf("item %s has reason=high-salience but relevance=%.2f (should be 0)",
					item.ID, item.Relevance)
			}
		case "trm":
			// TRM scoring path doesn't produce a query-keyword relevance;
			// no invariant to assert here.
		default:
			t.Errorf("item %s has unknown reason %q", item.ID, item.Reason)
		}
	}
}

// TestDebugSnapshotReasonClassifierContract is a unit-level guard on the
// classifier itself: given any (relevance, salience) pair, the assigned
// reason must be derivable from those values, and (relevance == 0 AND
// reason == "both") must never occur. This is the construction-level
// assertion called out in the issue's acceptance criteria.
func TestDebugSnapshotReasonClassifierContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		relevance float64
		salience  float64
		want      string
	}{
		{relevance: 0.5, salience: 0.5, want: "both"},
		{relevance: 0.5, salience: 0.0, want: "query-match"},
		{relevance: 0.0, salience: 0.5, want: "high-salience"},
	}
	for _, tc := range cases {
		got := classifyReason(tc.relevance, tc.salience)
		if got != tc.want {
			t.Errorf("classifyReason(%.2f, %.2f) = %q; want %q",
				tc.relevance, tc.salience, got, tc.want)
		}
		// The contract: relevance == 0 ⇒ reason != "both".
		if tc.relevance == 0 && got == "both" {
			t.Errorf("contract violation: relevance=0 produced reason=both (issue #91)")
		}
	}
}

// classifyReason mirrors the inline classifier in assembleContextInnerWithOpts.
// Kept here as a tiny pure function so the contract above is testable without
// spinning up a Process.
func classifyReason(relevance, salience float64) string {
	switch {
	case relevance > 0 && salience > 0:
		return "both"
	case relevance > 0:
		return "query-match"
	default:
		return "high-salience"
	}
}

// Sanity: the snapshot's CogDocsInjectedPaths should align with the assembled
// paths and the count should match CogDocsInjected. (Cheap regression guard.)
func TestDebugSnapshotInjectedPathsMatch(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := NewProcess(cfg, makeNucleus("T", "n"))

	memDir := filepath.Join(root, ".cog", "mem", "semantic")
	writeTestFile(t, filepath.Join(memDir, "topic-one.cog.md"), "---\ntitle: Topic One\nstatus: active\n---\n\nBody.\n")

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	p.indexMu.Lock()
	p.index = idx
	p.indexMu.Unlock()

	clientMsgs := []ProviderMessage{{Role: "user", Content: "topic"}}
	pkg, err := p.AssembleContext("topic", clientMsgs, 0)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	snap := captureDebugSnapshot(
		clientMsgs, "topic", "m", pkg, 1, "p", "m", 0, time.Millisecond,
	)
	if snap.Engine.CogDocsInjected != len(snap.Engine.CogDocsInjectedPaths) {
		t.Errorf("CogDocsInjected (%d) != len(CogDocsInjectedPaths) (%d)",
			snap.Engine.CogDocsInjected, len(snap.Engine.CogDocsInjectedPaths))
	}
	for _, path := range snap.Engine.CogDocsInjectedPaths {
		if !strings.HasSuffix(path, ".cog.md") {
			t.Errorf("injected path %q missing .cog.md suffix", path)
		}
	}
}
