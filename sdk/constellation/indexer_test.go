package constellation

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTempCogdoc creates a temporary cogdoc file with the given frontmatter and body.
// Returns the file path.
func writeTempCogdoc(t *testing.T, frontmatter, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.cog.md")
	content := "---\n" + frontmatter + "\n---\n\n" + body
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp cogdoc: %v", err)
	}
	return path
}

// TestParseCogdocMemorySectorAlias verifies that memory_sector maps to Sector (CRITICAL-1).
func TestParseCogdocMemorySectorAlias(t *testing.T) {
	path := writeTempCogdoc(t,
		"memory_sector: episodic\ntitle: Test\ncreated: 2026-01-01\ntype: note",
		"body content",
	)
	doc, err := parseCogdoc([]byte(
		"---\nmemory_sector: episodic\ntitle: Test\ncreated: 2026-01-01\ntype: note\n---\n\nbody content",
	), path)
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Sector != "episodic" {
		t.Errorf("expected Sector=episodic from memory_sector alias, got %q", doc.Sector)
	}
}

// TestParseCogdocMemorySectorNoOverride verifies that explicit sector wins over memory_sector.
func TestParseCogdocMemorySectorNoOverride(t *testing.T) {
	path := writeTempCogdoc(t, "", "")
	doc, err := parseCogdoc([]byte(
		"---\nsector: semantic\nmemory_sector: episodic\ntitle: Test\ncreated: 2026-01-01\ntype: note\n---\n\nbody",
	), path)
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Sector != "semantic" {
		t.Errorf("expected explicit sector=semantic to win over memory_sector, got %q", doc.Sector)
	}
}

// TestParseCogdocCogSubmap verifies cog.type and cog.id lift to top-level (CRITICAL-2).
func TestParseCogdocCogSubmap(t *testing.T) {
	path := writeTempCogdoc(t, "", "")
	doc, err := parseCogdoc([]byte(
		"---\ncog:\n  type: rfc\n  id: RFC-030\ntitle: Identity Contract\ncreated: 2026-04-30\n---\n\nbody",
	), path)
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Type != "rfc" {
		t.Errorf("expected Type=rfc from cog.type, got %q", doc.Type)
	}
	if doc.ID != "RFC-030" {
		t.Errorf("expected ID=RFC-030 from cog.id, got %q", doc.ID)
	}
}

// TestParseCogdocCogSubmapNoOverride verifies top-level type wins over cog submap.
func TestParseCogdocCogSubmapNoOverride(t *testing.T) {
	path := writeTempCogdoc(t, "", "")
	doc, err := parseCogdoc([]byte(
		"---\ntype: spec\nid: my-spec\ncog:\n  type: rfc\n  id: RFC-999\ntitle: Test\ncreated: 2026-04-30\n---\n\nbody",
	), path)
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Type != "spec" {
		t.Errorf("expected top-level type=spec to win over cog.type, got %q", doc.Type)
	}
	if doc.ID != "my-spec" {
		t.Errorf("expected top-level id=my-spec to win over cog.id, got %q", doc.ID)
	}
}

// TestParseCogdocStatusLowercase verifies status is lowercased at parse time (CRITICAL-3).
func TestParseCogdocStatusLowercase(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"Active", "active"},
		{"DRAFT", "draft"},
		{"Canonical", "canonical"},
		{"active", "active"},
		{"", ""},
	}
	for _, tc := range cases {
		path := writeTempCogdoc(t, "", "")
		doc, err := parseCogdoc([]byte(
			"---\ntype: note\ntitle: Test\ncreated: 2026-01-01\nstatus: "+tc.input+"\n---\n\nbody",
		), path)
		if err != nil {
			t.Fatalf("parseCogdoc failed for status=%q: %v", tc.input, err)
		}
		if doc.Status != tc.expected {
			t.Errorf("status %q: expected %q after lowercase, got %q", tc.input, tc.expected, doc.Status)
		}
	}
}

// TestParseCogdocUpdatedAliases verifies modified and revised alias for updated (CRITICAL-1).
func TestParseCogdocUpdatedAliases(t *testing.T) {
	path := writeTempCogdoc(t, "", "")

	// modified alias
	doc, err := parseCogdoc([]byte(
		"---\ntype: note\ntitle: T\ncreated: 2026-01-01\nmodified: 2026-04-30\n---\n\nbody",
	), path)
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Updated != "2026-04-30" {
		t.Errorf("expected Updated=2026-04-30 from modified alias, got %q", doc.Updated)
	}

	// revised alias
	doc, err = parseCogdoc([]byte(
		"---\ntype: note\ntitle: T\ncreated: 2026-01-01\nrevised: 2026-05-01\n---\n\nbody",
	), path)
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Updated != "2026-05-01" {
		t.Errorf("expected Updated=2026-05-01 from revised alias, got %q", doc.Updated)
	}
}

// TestParseCogdocCanonicalFields verifies salience, confidence, ingested are parsed (PREREQ-1).
func TestParseCogdocCanonicalFields(t *testing.T) {
	path := writeTempCogdoc(t, "", "")
	doc, err := parseCogdoc([]byte(
		"---\ntype: insight\ntitle: Test\ncreated: 2026-01-01\nsalience: high\nconfidence: empirical\ningested: 2026-05-01T00:00:00Z\n---\n\nbody",
	), path)
	if err != nil {
		t.Fatalf("parseCogdoc failed: %v", err)
	}
	if doc.Salience != "high" {
		t.Errorf("expected Salience=high, got %q", doc.Salience)
	}
	if doc.Confidence != "empirical" {
		t.Errorf("expected Confidence=empirical, got %q", doc.Confidence)
	}
	if doc.Ingested != "2026-05-01T00:00:00Z" {
		t.Errorf("expected Ingested=2026-05-01T00:00:00Z, got %q", doc.Ingested)
	}
}

// TestResolveURIMemoryNormalization verifies cog://memory/ → cog://mem/ normalization (PREREQ-2).
// We test indirectly by checking that both URIs resolve the same way via the function.
func TestResolveURIMemoryNormalization(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	tx, err := c.db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	// Insert a test document at a mem path
	_, err = tx.Exec(`INSERT INTO documents (id, path, type, title, created, content, content_hash, indexed_at, file_mtime)
		VALUES ('test-doc', '/workspace/.cog/mem/semantic/test.cog.md', 'note', 'Test', '2026-01-01', 'body', 'hash', '2026-01-01', '2026-01-01')`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Resolve via cog://memory/ (should normalize to cog://mem/)
	result := resolveURIToID(tx, "cog://memory/semantic/test")
	if !result.Valid {
		t.Error("expected cog://memory/semantic/test to resolve after normalization, got null")
	}

	// Also test cog://mem/ directly
	result2 := resolveURIToID(tx, "cog://mem/semantic/test")
	if !result2.Valid {
		t.Error("expected cog://mem/semantic/test to resolve, got null")
	}
}

// TestResolveURIRFCScheme verifies cog://rfc/NNN resolves via glob (PREREQ-2).
func TestResolveURIRFCScheme(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	tx, err := c.db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	// Insert RFC-030 at its canonical path
	_, err = tx.Exec(`INSERT INTO documents (id, path, type, title, created, content, content_hash, indexed_at, file_mtime)
		VALUES ('RFC-030', '/workspace/.cog/conf/spec/rfc/RFC-030-kernel-issued-cogdoc-identity-contract.cog.md',
		        'rfc', 'RFC-030', '2026-04-30', 'body', 'hash', '2026-04-30', '2026-04-30')`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	cases := []struct {
		uri    string
		wantID string
		wantOK bool
	}{
		{"cog://rfc/030", "RFC-030", true},
		{"cog://rfc/30", "RFC-030", true}, // zero-padded normalization
		{"cog://rfc/999", "", false},       // nonexistent RFC
	}

	for _, tc := range cases {
		result := resolveURIToID(tx, tc.uri)
		if result.Valid != tc.wantOK {
			t.Errorf("uri %q: valid=%v want %v", tc.uri, result.Valid, tc.wantOK)
			continue
		}
		if tc.wantOK && result.String != tc.wantID {
			t.Errorf("uri %q: id=%q want %q", tc.uri, result.String, tc.wantID)
		}
	}
}

// TestIndexWorkspaceStalePathPurge verifies stale cog-workspace paths are purged (CRITICAL-4).
func TestIndexWorkspaceStalePathPurge(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	// Insert a stale cog-workspace row directly
	_, err := c.db.Exec(`INSERT INTO documents (id, path, type, title, created, content, content_hash, indexed_at, file_mtime)
		VALUES ('stale-doc', '/Users/slowbro/cog-workspace/.cog/mem/test.cog.md',
		        'note', 'Stale', '2025-01-01', 'body', 'hash', '2025-01-01', '2025-01-01')`)
	if err != nil {
		t.Fatalf("insert stale doc: %v", err)
	}

	// Verify it's there
	var before int
	c.db.QueryRow("SELECT COUNT(*) FROM documents WHERE path LIKE '/Users/slowbro/cog-workspace/%'").Scan(&before)
	if before != 1 {
		t.Fatalf("expected 1 stale row before purge, got %d", before)
	}

	// Create the .cog directory structure required for WalkDir
	cogDir := filepath.Join(c.root, ".cog")
	if err := os.MkdirAll(cogDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Run index (will trigger purge + walk the empty .cog dir)
	_ = c.IndexWorkspace() // errors from empty .cog dir are acceptable

	// Verify stale row is gone
	var after int
	c.db.QueryRow("SELECT COUNT(*) FROM documents WHERE path LIKE '/Users/slowbro/cog-workspace/%'").Scan(&after)
	if after != 0 {
		t.Errorf("expected stale cog-workspace rows to be purged, got %d remaining", after)
	}
}

// TestNewColumnsExistAfterMigration verifies ingested, salience, confidence, display_number columns (PREREQ-1).
func TestNewColumnsExistAfterMigration(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	columns := []string{"ingested", "salience", "confidence", "display_number"}
	for _, col := range columns {
		var count int
		err := c.DB().QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('documents') WHERE name = ?`, col,
		).Scan(&count)
		if err != nil {
			t.Fatalf("pragma check for %s: %v", col, err)
		}
		if count == 0 {
			t.Errorf("expected column %q to exist after migration, but it's absent", col)
		}
	}
}

// TestMigrationConflictsTableExists verifies migration_conflicts table was created (CRITICAL-6).
func TestMigrationConflictsTableExists(t *testing.T) {
	c, cleanup := openTestDB(t)
	defer cleanup()

	var count int
	err := c.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='migration_conflicts'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("sqlite_master check: %v", err)
	}
	if count == 0 {
		t.Error("expected migration_conflicts table to exist")
	}
}
