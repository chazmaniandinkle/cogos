// identity_provider_test.go
// Covers IdentityProvider's full reconcile lifecycle (LoadConfig → FetchLive →
// ComputePlan → ApplyPlan → BuildState → Health) against an in-memory
// ConstellationDB fake and a captured event bus. Also exercises key
// resolvers, integrity-hash verification, and the three-axis health surface.

package main

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ─── In-memory fakes ─────────────────────────────────────────────────────────────

// memDB is a thread-safe in-memory ConstellationDB implementation for tests.
// The provider only depends on the six methods of ConstellationDB, so this
// single struct is enough to drive the whole reconcile lifecycle.
type memDB struct {
	mu           sync.Mutex
	projections  map[string]IdentityProjection
	participants map[string]ParticipantRow
}

func newMemDB() *memDB {
	return &memDB{
		projections:  make(map[string]IdentityProjection),
		participants: make(map[string]ParticipantRow),
	}
}

func (m *memDB) UpsertIdentityCogDoc(_ context.Context, doc IdentityProjection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.projections[doc.Sub] = doc
	return nil
}

func (m *memDB) DeleteIdentityCogDoc(_ context.Context, sub string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.projections, sub)
	return nil
}

func (m *memDB) UpsertParticipant(_ context.Context, row ParticipantRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.participants[row.ID] = row
	return nil
}

func (m *memDB) DeleteParticipant(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.participants, id)
	return nil
}

func (m *memDB) GetProjection(_ context.Context, sub string) (*IdentityProjection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.projections[sub]; ok {
		cp := p
		return &cp, nil
	}
	return nil, nil
}

func (m *memDB) ListProjections(_ context.Context) ([]IdentityProjection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]IdentityProjection, 0, len(m.projections))
	for _, p := range m.projections {
		out = append(out, p)
	}
	return out, nil
}

// capturedEvent is one entry in the test event bus.
type capturedEvent struct {
	Type string
	Data map[string]any
}

// eventRecorder is a BusEmit that appends into a slice under a mutex.
func eventRecorder(out *[]capturedEvent, mu *sync.Mutex) BusEmit {
	return func(t string, d map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		// Copy the map so test mutations don't affect history.
		cp := make(map[string]any, len(d))
		for k, v := range d {
			cp[k] = v
		}
		*out = append(*out, capturedEvent{Type: t, Data: cp})
		return nil
	}
}

// ─── Fixture builders ────────────────────────────────────────────────────────────

// writeIdentityCRD materializes a minimal valid identity YAML in the given
// workspace root. `extra` is appended to the spec block verbatim (trailing
// newline included) for tests that need to add private_key or auth_factors.
func writeIdentityCRD(t *testing.T, root, sub, iss, displayName, extra string) {
	t.Helper()
	dir := identityCRDDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := fmt.Sprintf(`apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: %s
spec:
  iss: %s
  sub: %s
  type: agent
%s  expressions:
    - aud: "*"
      display_name: %q
`, sub, iss, sub, extra, displayName)
	path := filepath.Join(dir, sub+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// setupProvider builds a fresh provider with a fresh in-memory DB and event
// recorder. Returns everything the tests commonly need.
type providerFixture struct {
	root   string
	db     *memDB
	prov   *IdentityProvider
	events *[]capturedEvent
	evMu   *sync.Mutex
}

func setupProvider(t *testing.T) *providerFixture {
	t.Helper()
	root := t.TempDir()
	db := newMemDB()
	var events []capturedEvent
	var mu sync.Mutex
	prov := NewIdentityProvider(db, nil, eventRecorder(&events, &mu))
	return &providerFixture{
		root:   root,
		db:     db,
		prov:   prov,
		events: &events,
		evMu:   &mu,
	}
}

// ─── Tests ───────────────────────────────────────────────────────────────────────

func TestIdentityProvider_Type(t *testing.T) {
	p := NewIdentityProvider(nil, nil, nil)
	if got := p.Type(); got != "identity" {
		t.Errorf("Type() = %q, want %q", got, "identity")
	}
}

func TestIdentityProvider_LoadConfig_Empty(t *testing.T) {
	fx := setupProvider(t)

	cfg, err := fx.prov.LoadConfig(fx.root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	idCfg, ok := cfg.(*identityConfig)
	if !ok {
		t.Fatalf("LoadConfig returned %T, want *identityConfig", cfg)
	}
	if len(idCfg.CRDs) != 0 {
		t.Errorf("CRDs len = %d, want 0", len(idCfg.CRDs))
	}
}

func TestIdentityProvider_LoadConfig_Multiple(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")
	writeIdentityCRD(t, fx.root, "claude", "cogos-dev", "Claude", "")
	writeIdentityCRD(t, fx.root, "sandy", "cogos-dev", "Sandy", "")

	cfg, err := fx.prov.LoadConfig(fx.root)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	idCfg := cfg.(*identityConfig)
	if len(idCfg.CRDs) != 3 {
		t.Fatalf("CRDs len = %d, want 3", len(idCfg.CRDs))
	}
	for _, sub := range []string{"cog", "claude", "sandy"} {
		h, ok := idCfg.SpecHash[sub]
		if !ok || !strings.HasPrefix(h, "sha256:") {
			t.Errorf("SpecHash[%q] = %q, want sha256:...", sub, h)
		}
	}
}

func TestIdentityProvider_FetchLive_EmptyDB(t *testing.T) {
	fx := setupProvider(t)

	live, err := fx.prov.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	snap := live.(*identityLive)
	if len(snap.Projections) != 0 {
		t.Errorf("Projections = %d, want 0", len(snap.Projections))
	}
}

func TestIdentityProvider_FetchLive_ExistingProjections(t *testing.T) {
	fx := setupProvider(t)
	// Prime the DB directly so FetchLive has something to read back.
	_ = fx.db.UpsertIdentityCogDoc(context.Background(), IdentityProjection{
		Sub: "cog", Iss: "cogos-dev", Type: "agent", SpecHash: "sha256:deadbeef",
	})

	live, err := fx.prov.FetchLive(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchLive: %v", err)
	}
	snap := live.(*identityLive)
	if got, ok := snap.Projections["cog"]; !ok || got.SpecHash != "sha256:deadbeef" {
		t.Errorf("Projections[cog] = %+v, want spec_hash sha256:deadbeef", got)
	}
}

func TestIdentityProvider_ComputePlan_AllCreates(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")
	writeIdentityCRD(t, fx.root, "claude", "cogos-dev", "Claude", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)

	plan, err := fx.prov.ComputePlan(cfg, live, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan.Summary.Creates != 2 {
		t.Errorf("Creates = %d, want 2", plan.Summary.Creates)
	}
	if plan.Summary.Updates != 0 || plan.Summary.Deletes != 0 {
		t.Errorf("Updates/Deletes = %d/%d, want 0/0", plan.Summary.Updates, plan.Summary.Deletes)
	}
}

func TestIdentityProvider_ComputePlan_NoChanges(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	// First pass: create.
	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}

	// Second pass: re-compute, should be all-skip.
	live2, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan2, err := fx.prov.ComputePlan(cfg, live2, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan2.Summary.HasChanges() {
		t.Errorf("expected no changes, got %+v", plan2.Summary)
	}
	if plan2.Summary.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", plan2.Summary.Skipped)
	}
}

func TestIdentityProvider_ComputePlan_ExpressionUpdate(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}

	// Change display_name.
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog v2", "")

	cfg2, _ := fx.prov.LoadConfig(fx.root)
	live2, _ := fx.prov.FetchLive(context.Background(), cfg2)
	plan2, err := fx.prov.ComputePlan(cfg2, live2, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan2.Summary.Updates != 1 {
		t.Errorf("Updates = %d, want 1", plan2.Summary.Updates)
	}
}

func TestIdentityProvider_ComputePlan_Delete(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	// Apply create.
	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}

	// Remove the spec file.
	if err := os.Remove(filepath.Join(identityCRDDir(fx.root), "cog.yaml")); err != nil {
		t.Fatalf("remove spec: %v", err)
	}

	cfg2, _ := fx.prov.LoadConfig(fx.root)
	live2, _ := fx.prov.FetchLive(context.Background(), cfg2)
	plan2, err := fx.prov.ComputePlan(cfg2, live2, nil)
	if err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	if plan2.Summary.Deletes != 1 {
		t.Errorf("Deletes = %d, want 1", plan2.Summary.Deletes)
	}
}

func TestIdentityProvider_ApplyPlan_Create_EmitsProjectedEvent(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	results, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	if len(results) != 1 || results[0].Status != ApplySucceeded {
		t.Fatalf("results = %+v, want 1 succeeded", results)
	}

	// The projection cogdoc must exist on disk.
	if _, err := os.Stat(filepath.Join(fx.root, ".cog", "id", "cog.cog.md")); err != nil {
		t.Errorf("projection cogdoc not written: %v", err)
	}
	// DB has the participant row.
	if _, ok := fx.db.participants["agent:cog"]; !ok {
		t.Errorf("participants[agent:cog] missing")
	}
	// Event emitted.
	assertEmitted(t, fx, EventIdentityProjected, 1)
}

func TestIdentityProvider_ApplyPlan_UpdateEmitsExpressionUpdatedEvent(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}
	// Reset events so the update assertion is unambiguous.
	fx.evMu.Lock()
	*fx.events = nil
	fx.evMu.Unlock()

	// Change the spec, re-plan, apply as update.
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog v2", "")
	cfg2, _ := fx.prov.LoadConfig(fx.root)
	live2, _ := fx.prov.FetchLive(context.Background(), cfg2)
	plan2, _ := fx.prov.ComputePlan(cfg2, live2, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan2); err != nil {
		t.Fatalf("ApplyPlan (update): %v", err)
	}

	assertEmitted(t, fx, EventIdentityExpressionUpdated, 1)
}

func TestIdentityProvider_ApplyPlan_DeleteEmitsDeregisteredEvent(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan (create): %v", err)
	}
	fx.evMu.Lock()
	*fx.events = nil
	fx.evMu.Unlock()

	// Remove the spec, re-plan, apply as delete.
	if err := os.Remove(filepath.Join(identityCRDDir(fx.root), "cog.yaml")); err != nil {
		t.Fatalf("remove spec: %v", err)
	}
	cfg2, _ := fx.prov.LoadConfig(fx.root)
	live2, _ := fx.prov.FetchLive(context.Background(), cfg2)
	plan2, _ := fx.prov.ComputePlan(cfg2, live2, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan2); err != nil {
		t.Fatalf("ApplyPlan (delete): %v", err)
	}

	assertEmitted(t, fx, EventIdentityDeregistered, 1)
	// Participant row removed.
	if _, ok := fx.db.participants["agent:cog"]; ok {
		t.Errorf("participants[agent:cog] still present after delete")
	}
	// Projection cogdoc removed from disk.
	if _, err := os.Stat(filepath.Join(fx.root, ".cog", "id", "cog.cog.md")); !os.IsNotExist(err) {
		t.Errorf("projection cogdoc still on disk after delete: err=%v", err)
	}
}

func TestIdentityProvider_ApplyPlan_KeyHashMismatch_RefusesApply_EmitsMismatchEvent(t *testing.T) {
	fx := setupProvider(t)

	// Write a real key bytes file, but state a bogus hash in the CRD.
	keyBytes := []byte("fake-pem-bytes")
	keyPath := filepath.Join(fx.root, "key.pem")
	if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	extra := fmt.Sprintf(`  private_key:
    ref: "file://%s"
    integrity_hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
`, keyPath)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", extra)

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	results, err := fx.prov.ApplyPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyPlan returned top-level error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Status != ApplyFailed {
		t.Errorf("Status = %q, want %q", results[0].Status, ApplyFailed)
	}
	if !strings.Contains(results[0].Error, "mismatch") {
		t.Errorf("Error = %q, want substring 'mismatch'", results[0].Error)
	}

	assertEmitted(t, fx, EventIdentityKeyMismatch, 1)
	// Projection cogdoc must NOT have been written.
	if _, err := os.Stat(filepath.Join(fx.root, ".cog", "id", "cog.cog.md")); !os.IsNotExist(err) {
		t.Errorf("projection cogdoc written despite mismatch: err=%v", err)
	}
	// DB must NOT have a row.
	if _, ok := fx.db.projections["cog"]; ok {
		t.Errorf("projection in DB despite mismatch")
	}
}

func TestIdentityProvider_KeyResolvers_FileScheme(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "k.pem")
	want := []byte("hello")
	if err := os.WriteFile(keyPath, want, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := (fileKeyResolver{}).ResolveKey(context.Background(), "file://"+keyPath)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestIdentityProvider_KeyResolvers_InlineScheme(t *testing.T) {
	payload := []byte("inline-data")
	ref := "inline://" + base64.StdEncoding.EncodeToString(payload)
	got, err := (inlineKeyResolver{}).ResolveKey(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveKey: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestIdentityProvider_KeyResolvers_UnsupportedScheme_Errors(t *testing.T) {
	p := NewIdentityProvider(nil, nil, nil)
	for _, scheme := range []string{"vault", "s3", "keychain", "env", "kms"} {
		ref := scheme + "://whatever"
		if _, err := p.resolveKey(context.Background(), ref); !errors.Is(err, ErrSchemeNotImplemented) {
			t.Errorf("%s://: err = %v, want ErrSchemeNotImplemented", scheme, err)
		}
	}
}

func TestIdentityProvider_IntegrityHash_SHA256(t *testing.T) {
	data := []byte("some-key-bytes")
	sum := sha256.Sum256(data)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	if err := verifyKeyIntegrity(data, hash); err != nil {
		t.Errorf("verifyKeyIntegrity passed-case: %v", err)
	}

	// sha512 passes too.
	sum5 := sha512.Sum512(data)
	hash5 := "sha512:" + hex.EncodeToString(sum5[:])
	if err := verifyKeyIntegrity(data, hash5); err != nil {
		t.Errorf("verifyKeyIntegrity sha512: %v", err)
	}
}

func TestIdentityProvider_IntegrityHash_Mismatch(t *testing.T) {
	data := []byte("some-key-bytes")
	bogus := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := verifyKeyIntegrity(data, bogus); err == nil {
		t.Errorf("verifyKeyIntegrity(mismatch) returned nil, want error")
	}
}

func TestIdentityProvider_Health_ThreeAxes(t *testing.T) {
	// Scenario 1: fresh provider with no config → Synced / Healthy / Idle.
	p := NewIdentityProvider(newMemDB(), nil, nil)
	got := p.Health()
	if got.Sync != SyncStatusSynced || got.Health != HealthHealthy || got.Operation != OperationIdle {
		t.Errorf("scenario 1: %+v, want Synced/Healthy/Idle", got)
	}

	// Scenario 2: plan has pending creates → OutOfSync / Healthy / Idle.
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")
	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	if _, err := fx.prov.ComputePlan(cfg, live, nil); err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	got = fx.prov.Health()
	if got.Sync != SyncStatusOutOfSync {
		t.Errorf("scenario 2: Sync = %q, want OutOfSync", got.Sync)
	}
	if got.Health != HealthHealthy {
		t.Errorf("scenario 2: Health = %q, want Healthy", got.Health)
	}

	// Scenario 3: projection present but spec deleted → Health = Missing.
	fx3 := setupProvider(t)
	writeIdentityCRD(t, fx3.root, "cog", "cogos-dev", "Cog", "")
	cfg3, _ := fx3.prov.LoadConfig(fx3.root)
	live3, _ := fx3.prov.FetchLive(context.Background(), cfg3)
	plan3, _ := fx3.prov.ComputePlan(cfg3, live3, nil)
	if _, err := fx3.prov.ApplyPlan(context.Background(), plan3); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	// Remove spec, re-plan; projection now orphaned.
	_ = os.Remove(filepath.Join(identityCRDDir(fx3.root), "cog.yaml"))
	cfg3b, _ := fx3.prov.LoadConfig(fx3.root)
	live3b, _ := fx3.prov.FetchLive(context.Background(), cfg3b)
	if _, err := fx3.prov.ComputePlan(cfg3b, live3b, nil); err != nil {
		t.Fatalf("ComputePlan: %v", err)
	}
	got = fx3.prov.Health()
	if got.Health != HealthMissing {
		t.Errorf("scenario 3: Health = %q, want Missing", got.Health)
	}

	// Scenario 4: key-hash mismatch → Health = Degraded.
	fx4 := setupProvider(t)
	keyBytes := []byte("fake-pem-bytes")
	keyPath := filepath.Join(fx4.root, "k.pem")
	_ = os.WriteFile(keyPath, keyBytes, 0o600)
	extra := fmt.Sprintf(`  private_key:
    ref: "file://%s"
    integrity_hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
`, keyPath)
	writeIdentityCRD(t, fx4.root, "cog", "cogos-dev", "Cog", extra)
	cfg4, _ := fx4.prov.LoadConfig(fx4.root)
	live4, _ := fx4.prov.FetchLive(context.Background(), cfg4)
	plan4, _ := fx4.prov.ComputePlan(cfg4, live4, nil)
	_, _ = fx4.prov.ApplyPlan(context.Background(), plan4)
	got = fx4.prov.Health()
	if got.Health != HealthDegraded {
		t.Errorf("scenario 4: Health = %q, want Degraded", got.Health)
	}
}

func TestIdentityProvider_BuildState_LineageSurvives(t *testing.T) {
	fx := setupProvider(t)
	writeIdentityCRD(t, fx.root, "cog", "cogos-dev", "Cog", "")

	cfg, _ := fx.prov.LoadConfig(fx.root)
	live, _ := fx.prov.FetchLive(context.Background(), cfg)
	plan, _ := fx.prov.ComputePlan(cfg, live, nil)
	if _, err := fx.prov.ApplyPlan(context.Background(), plan); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	live1, _ := fx.prov.FetchLive(context.Background(), cfg)
	state1, err := fx.prov.BuildState(cfg, live1, nil)
	if err != nil {
		t.Fatalf("BuildState #1: %v", err)
	}
	if state1.Lineage == "" {
		t.Fatalf("state1.Lineage empty")
	}
	if state1.Serial != 1 {
		t.Errorf("state1.Serial = %d, want 1", state1.Serial)
	}

	// Rebuild with the previous state as existing.
	state2, err := fx.prov.BuildState(cfg, live1, state1)
	if err != nil {
		t.Fatalf("BuildState #2: %v", err)
	}
	if state2.Lineage != state1.Lineage {
		t.Errorf("Lineage changed: %q → %q", state1.Lineage, state2.Lineage)
	}
	if state2.Serial != state1.Serial+1 {
		t.Errorf("Serial = %d, want %d", state2.Serial, state1.Serial+1)
	}
	if len(state2.Resources) != 1 {
		t.Errorf("Resources len = %d, want 1", len(state2.Resources))
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────────

// assertEmitted verifies that exactly `want` events of type `eventType` were
// captured by the fixture's recorder.
func assertEmitted(t *testing.T, fx *providerFixture, eventType string, want int) {
	t.Helper()
	fx.evMu.Lock()
	defer fx.evMu.Unlock()
	got := 0
	for _, e := range *fx.events {
		if e.Type == eventType {
			got++
		}
	}
	if got != want {
		t.Errorf("event %q: got %d, want %d (all events: %v)", eventType, got, want, eventTypes(*fx.events))
	}
}

func eventTypes(evs []capturedEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}
