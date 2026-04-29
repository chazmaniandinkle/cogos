package engine

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNewAttentionalField(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	if f.Len() != 0 {
		t.Errorf("new field Len = %d; want 0", f.Len())
	}
	if !f.LastUpdated().IsZero() {
		t.Error("new field LastUpdated should be zero")
	}
}

func TestFieldFovea(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	// Inject scores directly (white-box access is fine in package main tests).
	scores := map[string]float64{
		"/a.md": 0.9,
		"/b.md": 0.5,
		"/c.md": 0.7,
		"/d.md": 0.1,
	}
	f.mu.Lock()
	f.base = scores
	f.observer = copyScoreMap(scores)
	f.lastUpdated = time.Now()
	f.mu.Unlock()

	// Fovea(2) should return the top-2.
	top2 := f.Fovea(2)
	if len(top2) != 2 {
		t.Fatalf("Fovea(2) len = %d; want 2", len(top2))
	}
	if top2[0].Score != 0.9 {
		t.Errorf("Fovea[0].Score = %.2f; want 0.9", top2[0].Score)
	}
	if top2[1].Score != 0.7 {
		t.Errorf("Fovea[1].Score = %.2f; want 0.7", top2[1].Score)
	}
}

func TestFieldFoveaAll(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	f.mu.Lock()
	f.base = map[string]float64{"/x.md": 0.5, "/y.md": 0.3}
	f.observer = copyScoreMap(f.base)
	f.mu.Unlock()

	// n=0 returns all.
	all := f.Fovea(0)
	if len(all) != 2 {
		t.Errorf("Fovea(0) len = %d; want 2", len(all))
	}
}

func TestFieldScore(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	f.mu.Lock()
	f.base = map[string]float64{"/known.md": 0.42}
	f.observer = copyScoreMap(f.base)
	f.mu.Unlock()

	if got := f.Score("/known.md"); got != 0.42 {
		t.Errorf("Score known = %.2f; want 0.42", got)
	}
	if got := f.Score("/missing.md"); got != 0.0 {
		t.Errorf("Score missing = %.2f; want 0.0", got)
	}
}

func TestFieldUpdateEmptyWorkspace(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	f := NewAttentionalField(cfg)

	// With no git history, Update should not error
	// (files may just score 0 or not appear at all).
	if err := f.Update(); err != nil {
		t.Errorf("Update on empty workspace: %v", err)
	}
	if f.LastUpdated().IsZero() {
		t.Error("LastUpdated still zero after Update")
	}
}

func TestFieldConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	f.mu.Lock()
	f.base = map[string]float64{"/file.md": 0.5}
	f.observer = copyScoreMap(f.base)
	f.mu.Unlock()

	var wg sync.WaitGroup
	const readers = 10
	wg.Add(readers + 1)

	// One writer.
	go func() {
		defer wg.Done()
		for i := range 20 {
			f.mu.Lock()
			f.base["/file.md"] = float64(i) / 20.0
			f.observer["/file.md"] = float64(i) / 20.0
			f.mu.Unlock()
		}
	}()

	// Multiple concurrent readers.
	for range readers {
		go func() {
			defer wg.Done()
			for range 20 {
				_ = f.Score("/file.md")
				_ = f.ObserverScore("/file.md")
				_ = f.Len()
				_ = f.Fovea(5)
			}
		}()
	}

	wg.Wait()
}

// TestFieldInboxBoostSplit verifies that the chat-read view (Score) does
// NOT see inbox boosts, while the observer view (ObserverScore) does. This
// is the core invariant of issue #90 — inbox urgency should not contaminate
// the foveated chat-context assembler.
func TestFieldInboxBoostSplit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rawPath := filepath.Join(dir, "inbox", "raw.cog.md")
	enrichedPath := filepath.Join(dir, "inbox", "enriched.cog.md")
	nonInboxPath := filepath.Join(dir, "semantic", "knowledge", "topic.cog.md")

	mustWrite(t, rawPath, "---\nstatus: raw\n---\nbody\n")
	mustWrite(t, enrichedPath, "---\nstatus: enriched\n---\nbody\n")
	mustWrite(t, nonInboxPath, "---\nstatus: integrated\n---\nbody\n")

	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	// Build a base map with identical scores for all three files, then
	// produce the observer map by applying inbox boosts. This mirrors
	// what Update() does end-to-end without needing a populated git repo.
	const baseScore = 0.4
	base := map[string]float64{
		rawPath:      baseScore,
		enrichedPath: baseScore,
		nonInboxPath: baseScore,
	}
	observer := copyScoreMap(base)
	applyInboxBoosts(observer)

	f.mu.Lock()
	f.base = base
	f.observer = observer
	f.mu.Unlock()

	const eps = 1e-9

	// Inbox raw: ObserverScore exceeds Score by exactly inboxRawBoost.
	if got := f.Score(rawPath); got != baseScore {
		t.Errorf("raw Score = %.4f; want %.4f (no boost in chat-read view)", got, baseScore)
	}
	if delta := f.ObserverScore(rawPath) - f.Score(rawPath); !floatEq(delta, inboxRawBoost, eps) {
		t.Errorf("raw ObserverScore - Score = %.6f; want exactly inboxRawBoost (%.6f)",
			delta, inboxRawBoost)
	}

	// Inbox enriched: ObserverScore exceeds Score by exactly inboxEnrichedBoost.
	if got := f.Score(enrichedPath); got != baseScore {
		t.Errorf("enriched Score = %.4f; want %.4f", got, baseScore)
	}
	if delta := f.ObserverScore(enrichedPath) - f.Score(enrichedPath); !floatEq(delta, inboxEnrichedBoost, eps) {
		t.Errorf("enriched ObserverScore - Score = %.6f; want exactly inboxEnrichedBoost (%.6f)",
			delta, inboxEnrichedBoost)
	}

	// Non-inbox file: Score and ObserverScore agree exactly.
	if f.Score(nonInboxPath) != f.ObserverScore(nonInboxPath) {
		t.Errorf("non-inbox file: Score (%.4f) != ObserverScore (%.4f); want equal",
			f.Score(nonInboxPath), f.ObserverScore(nonInboxPath))
	}
}

func floatEq(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// TestFieldBoostAffectsBothViews verifies that recency boosts (CogDoc
// reads, attention signals, observer warming) propagate to both views
// — recency is real activity and should influence chat-read salience
// just as it does observer salience.
func TestFieldBoostAffectsBothViews(t *testing.T) {
	t.Parallel()
	cfg := makeConfig(t, t.TempDir())
	f := NewAttentionalField(cfg)

	const path = "/some/file.md"
	f.mu.Lock()
	f.base = map[string]float64{path: 0.10}
	f.observer = map[string]float64{path: 0.60} // pretend +0.5 raw boost
	f.mu.Unlock()

	f.Boost(path, 0.25)

	if got := f.Score(path); got != 0.35 {
		t.Errorf("Score after boost = %.4f; want 0.35", got)
	}
	if got := f.ObserverScore(path); got != 0.85 {
		t.Errorf("ObserverScore after boost = %.4f; want 0.85", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
