// identity_crd_test.go
// Covers: CRD YAML parsing, validation invariants, LoadIdentityCRDs
// directory enumeration, and the ExpressionFor collapse operation.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ─── YAML parsing + validation ──────────────────────────────────────────────────

const minimalValidIdentityYAML = `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: cog
spec:
  iss: cogos-dev
  sub: cog-pattern
  type: agent
  expressions:
    - aud: workspace:cog-workspace
      display_name: "Cog — Workspace Guardian"
`

func writeTempCRD(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLoadIdentityCRD_MinimalValid(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	if err := os.MkdirAll(idDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeTempCRD(t, idDir, "cog.yaml", minimalValidIdentityYAML)

	crd, err := LoadIdentityCRD(dir, "cog")
	if err != nil {
		t.Fatalf("LoadIdentityCRD: %v", err)
	}
	if crd.APIVersion != IdentityAPIVersion {
		t.Errorf("APIVersion = %q, want %q", crd.APIVersion, IdentityAPIVersion)
	}
	if crd.Spec.Issuer != "cogos-dev" {
		t.Errorf("iss = %q", crd.Spec.Issuer)
	}
	if crd.Spec.Subject != "cog-pattern" {
		t.Errorf("sub = %q", crd.Spec.Subject)
	}
	if len(crd.Spec.Expressions) != 1 {
		t.Fatalf("expressions len = %d, want 1", len(crd.Spec.Expressions))
	}
	if crd.Spec.Expressions[0].Audience != "workspace:cog-workspace" {
		t.Errorf("aud = %q", crd.Spec.Expressions[0].Audience)
	}
}

func TestLoadIdentityCRD_RejectsBadKind(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "bad.yaml", `
apiVersion: cog.os/v1alpha1
kind: Widget
metadata:
  name: bad
spec:
  iss: cogos-dev
  sub: bad
  type: agent
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "bad"); err == nil {
		t.Fatal("expected error for wrong kind, got nil")
	}
}

func TestLoadIdentityCRD_RejectsMissingIssOrSub(t *testing.T) {
	cases := map[string]string{
		"missing iss": `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  sub: x
  type: agent
  expressions:
    - aud: "*"
`,
		"missing sub": `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  type: agent
  expressions:
    - aud: "*"
`,
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			idDir := filepath.Join(dir, ".cog", "config", "identities")
			_ = os.MkdirAll(idDir, 0o755)
			writeTempCRD(t, idDir, "x.yaml", body)
			if _, err := LoadIdentityCRD(dir, "x"); err == nil {
				t.Fatalf("expected error for %s, got nil", label)
			}
		})
	}
}

func TestLoadIdentityCRD_RejectsBadType(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: robot
  expressions:
    - aud: "*"
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for bad type, got nil")
	}
}

func TestLoadIdentityCRD_RejectsEmptyExpressions(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  expressions: []
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for empty expressions, got nil")
	}
}

func TestLoadIdentityCRD_RejectsDuplicateAudience(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)
	writeTempCRD(t, idDir, "x.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: x
spec:
  iss: cogos-dev
  sub: x
  type: agent
  expressions:
    - aud: workspace:foo
      display_name: first
    - aud: workspace:foo
      display_name: second
`)
	if _, err := LoadIdentityCRD(dir, "x"); err == nil {
		t.Fatal("expected error for duplicate aud, got nil")
	}
}

// ─── Directory enumeration ──────────────────────────────────────────────────────

func TestLoadIdentityCRDs_EmptyWhenDirMissing(t *testing.T) {
	out, err := LoadIdentityCRDs(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentityCRDs: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(out))
	}
}

func TestLoadIdentityCRDs_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)

	writeTempCRD(t, idDir, "cog.yaml", minimalValidIdentityYAML)
	writeTempCRD(t, idDir, "sandy.yaml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: sandy
spec:
  iss: cogos-dev
  sub: sandy
  type: agent
  expressions:
    - aud: workspace:cog-workspace
      display_name: "Sandy — Lab Engineer"
`)
	// A .yml alias should also be picked up.
	writeTempCRD(t, idDir, "chaz.yml", `
apiVersion: cog.os/v1alpha1
kind: Identity
metadata:
  name: chaz
spec:
  iss: cogos-dev
  sub: chaz
  type: human
  expressions:
    - aud: "*"
      display_name: Chaz
`)
	// A non-yaml file should be ignored.
	writeTempCRD(t, idDir, "README.md", "# ignore me\n")

	out, err := LoadIdentityCRDs(dir)
	if err != nil {
		t.Fatalf("LoadIdentityCRDs: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d CRDs, want 3", len(out))
	}
	// Deterministic order (sorted by filename).
	wantOrder := []string{"chaz", "cog", "sandy"}
	for i, crd := range out {
		if crd.Metadata.Name != wantOrder[i] {
			t.Errorf("out[%d].Name = %q, want %q", i, crd.Metadata.Name, wantOrder[i])
		}
	}
}

func TestLoadIdentityCRDs_CollectsErrorsContinuesLoading(t *testing.T) {
	dir := t.TempDir()
	idDir := filepath.Join(dir, ".cog", "config", "identities")
	_ = os.MkdirAll(idDir, 0o755)

	writeTempCRD(t, idDir, "good.yaml", minimalValidIdentityYAML)
	writeTempCRD(t, idDir, "bad.yaml", `apiVersion: cog.os/v1alpha1
kind: Identity
spec:
  iss: cogos-dev
`) // missing metadata.name, missing sub, missing type, empty expressions

	out, err := LoadIdentityCRDs(dir)
	if err == nil {
		t.Fatal("expected error for bad.yaml, got nil")
	}
	if len(out) != 1 {
		t.Errorf("expected 1 good CRD loaded, got %d", len(out))
	}
}

// ─── Collapse operation (ExpressionFor) ─────────────────────────────────────────

func TestExpressionFor_ExactMatch(t *testing.T) {
	spec := &IdentityCRDSpec{
		Expressions: []IdentityExpression{
			{Audience: "workspace:a", DisplayName: "A"},
			{Audience: "workspace:b", DisplayName: "B"},
		},
	}
	got := spec.ExpressionFor("workspace:b")
	if got == nil || got.DisplayName != "B" {
		t.Fatalf("got %+v, want DisplayName=B", got)
	}
}

func TestExpressionFor_WildcardFallback(t *testing.T) {
	spec := &IdentityCRDSpec{
		Expressions: []IdentityExpression{
			{Audience: "workspace:a", DisplayName: "A"},
			{Audience: "*", DisplayName: "Fallback"},
		},
	}
	got := spec.ExpressionFor("channel:xyz")
	if got == nil || got.DisplayName != "Fallback" {
		t.Fatalf("got %+v, want DisplayName=Fallback", got)
	}
}

func TestExpressionFor_ExactBeatsWildcard(t *testing.T) {
	spec := &IdentityCRDSpec{
		Expressions: []IdentityExpression{
			{Audience: "*", DisplayName: "Fallback"},
			{Audience: "workspace:a", DisplayName: "A"},
		},
	}
	got := spec.ExpressionFor("workspace:a")
	if got == nil || got.DisplayName != "A" {
		t.Fatalf("got %+v, want DisplayName=A (exact should beat *)", got)
	}
}

func TestExpressionFor_NoMatchNoWildcard(t *testing.T) {
	spec := &IdentityCRDSpec{
		Expressions: []IdentityExpression{
			{Audience: "workspace:a", DisplayName: "A"},
		},
	}
	got := spec.ExpressionFor("workspace:b")
	if got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}

func TestExpressionFor_NilSpec(t *testing.T) {
	var spec *IdentityCRDSpec
	if got := spec.ExpressionFor("anything"); got != nil {
		t.Errorf("nil spec should return nil, got %+v", got)
	}
}
