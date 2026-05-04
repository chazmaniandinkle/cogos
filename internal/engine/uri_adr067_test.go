package engine

// uri_adr067_test.go — tests for ADR-067 URI compliance
//
// Covers:
//  1. Both cog: (bare) and cog:// (authority/legacy) forms accepted everywhere.
//  2. Digest fail-closed contract: ?digest=sha256:... MUST return ErrDigestNotVerified.
//  3. Round-trip equivalence: parse → emit → parse gives identical resolution.
//  4. ErrUnknownAuthority for cross-workspace authority URIs (registry absent until #167).

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ── 1. Both forms accepted ────────────────────────────────────────────────────

func TestResolveURI_BareFormAccepted(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	cases := []struct {
		bare      string
		authority string
		wantPath  string
	}{
		{
			bare:      "cog:mem/semantic/insights/foo.cog.md",
			authority: "cog://mem/semantic/insights/foo.cog.md",
			wantPath:  root + "/.cog/mem/semantic/insights/foo.cog.md",
		},
		{
			bare:      "cog:conf/kernel.yaml",
			authority: "cog://conf/kernel.yaml",
			wantPath:  root + "/.cog/config/kernel.yaml",
		},
		{
			bare:      "cog:crystal",
			authority: "cog://crystal",
			wantPath:  root + "/.cog/ledger/crystal.json",
		},
		{
			bare:      "cog:spec/my-spec",
			authority: "cog://spec/my-spec",
			wantPath:  root + "/.cog/specs/my-spec.cog.md",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.bare, func(t *testing.T) {
			t.Parallel()

			resBare, err := ResolveURI(root, tc.bare)
			if err != nil {
				t.Fatalf("bare form ResolveURI(%q): %v", tc.bare, err)
			}

			resAuth, err := ResolveURI(root, tc.authority)
			if err != nil {
				t.Fatalf("authority form ResolveURI(%q): %v", tc.authority, err)
			}

			if resBare.Path != tc.wantPath {
				t.Errorf("bare Path = %q; want %q", resBare.Path, tc.wantPath)
			}
			if resAuth.Path != tc.wantPath {
				t.Errorf("authority Path = %q; want %q", resAuth.Path, tc.wantPath)
			}
			// Both forms must resolve to the same path.
			if resBare.Path != resAuth.Path {
				t.Errorf("form mismatch: bare=%q authority=%q", resBare.Path, resAuth.Path)
			}
		})
	}
}

// ── 2. Digest fail-closed ─────────────────────────────────────────────────────

func TestResolveURI_DigestFailClosed(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	digests := []string{
		"cog:mem/semantic/foo.cog.md?digest=sha256:abc123",
		"cog://mem/semantic/foo.cog.md?digest=sha256:abc123",
		"cog:conf/kernel.yaml?digest=sha256:deadbeef",
		// digest in combination with other params
		"cog:mem/foo.cog.md?ref=main&digest=sha256:111",
	}

	for _, uri := range digests {
		uri := uri
		t.Run(uri, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveURI(root, uri)
			if err == nil {
				t.Fatalf("ResolveURI(%q): expected ErrDigestNotVerified, got nil", uri)
			}
			if !errors.Is(err, ErrDigestNotVerified) {
				t.Errorf("ResolveURI(%q): got %v; want errors.Is ErrDigestNotVerified", uri, err)
			}
		})
	}
}

func TestResolveURI_NonDigestQueryAllowed(t *testing.T) {
	t.Parallel()
	root := "/workspace"
	// Non-digest query params must NOT trigger the fail-closed error.
	_, err := ResolveURI(root, "cog:mem/semantic/foo.cog.md?ref=main")
	if err != nil {
		t.Errorf("ResolveURI with non-digest query: unexpected error %v", err)
	}
}

// ── 3. Round-trip equivalence ─────────────────────────────────────────────────

// roundTripMapping describes one entry in uriMappings that supports a clean
// PathToURI → ResolveURI round-trip.  Only "direct"-pattern projections
// round-trip cleanly: "directory" projections append a trailing separator on
// resolve (so the round-trip path differs from the input), "glob" projections
// require matching files on disk and are tested separately, and "singleton"
// projections have no path component.
//
// The table is derived from uriMappings (which drives PathToURI) cross-checked
// against projections (which drives ResolveURI).  Each entry carries a
// representative relative path under the mapping's filesystem prefix and the
// canonical URI that PathToURI must emit for it.
//
// Maintenance: when a new namespace is added to pkg/uri.Namespaces *and* has a
// "direct"-pattern projection in internal/engine/uri.go, add a row here.  The
// namespace_sync_test.go drift guard (issue #178) will catch Namespaces-only
// additions; this table catches projection-only additions (PathToURI mismatch).
var roundTripMappings = []struct {
	// name is the subtest label — conventionally the namespace key.
	name string
	// relPath is workspace-relative (no leading slash).  It is joined with
	// root inside the test.
	relPath string
	// wantURI is the exact cog: URI that PathToURI must emit for relPath.
	wantURI string
}{
	// ── Memory corpus ──────────────────────────────────────────────────────────
	{
		name:    "mem",
		relPath: ".cog/mem/semantic/insights/foo.cog.md",
		wantURI: "cog:mem/semantic/insights/foo.cog.md",
	},
	// ── Config ─────────────────────────────────────────────────────────────────
	// PathToURI emits the canonical "conf" alias; ResolveURI accepts it.
	{
		name:    "conf",
		relPath: ".cog/config/kernel.yaml",
		wantURI: "cog:conf/kernel.yaml",
	},
	// ── Ontology ───────────────────────────────────────────────────────────────
	// uriMappings strips .cog.md; the "direct" projection in ResolveURI re-adds
	// it, so the original path is recovered exactly.
	{
		name:    "ontology",
		relPath: ".cog/ontology/crystal.cog.md",
		wantURI: "cog:ontology/crystal",
	},
	// ── Docs ───────────────────────────────────────────────────────────────────
	{
		name:    "docs",
		relPath: ".cog/docs/framework-status.md",
		wantURI: "cog:docs/framework-status.md",
	},
	// ── Hooks ──────────────────────────────────────────────────────────────────
	{
		name:    "hooks",
		relPath: ".cog/hooks/session-start.sh",
		wantURI: "cog:hooks/session-start.sh",
	},
	// ── Specs ──────────────────────────────────────────────────────────────────
	// PathToURI emits the plural "specs" alias; ResolveURI accepts it.
	// uriMappings strips .cog.md; "direct+Suffix" re-adds it.
	{
		name:    "specs",
		relPath: ".cog/specs/my-spec.cog.md",
		wantURI: "cog:specs/my-spec",
	},
	// ── Status ─────────────────────────────────────────────────────────────────
	// uriMappings strips .json; "direct+Suffix" re-adds it.
	{
		name:    "status",
		relPath: ".cog/status/kernel.json",
		wantURI: "cog:status/kernel",
	},
	// ── Work ───────────────────────────────────────────────────────────────────
	{
		name:    "work",
		relPath: ".cog/work/sprint-1.md",
		wantURI: "cog:work/sprint-1.md",
	},
}

// TestPathToURI_RoundTrip verifies parse → emit → parse round-trip equivalence
// for every "direct"-pattern projection.  Cases are generated from
// roundTripMappings so coverage automatically tracks the source of truth in
// uriMappings / projections — adding a new direct-pattern namespace and row to
// roundTripMappings is sufficient; no manual subtest boilerplate needed.
//
// Round-trip contract (per ADR-067):
//
//	PathToURI(root, absPath)  →  uri          (emit: path → URI)
//	ResolveURI(root, uri)     →  res.Path     (parse: URI → path)
//	res.Path == absPath                        (invariant)
//
// "directory"- and "glob"-pattern projections are excluded: directory projections
// append a trailing separator on resolution (the round-trip path differs from
// the input), and glob projections require matching files on disk.  Both are
// exercised by other tests in uri_test.go.
func TestPathToURI_RoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	for _, m := range roundTripMappings {
		m := m
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()

			absPath := filepath.Join(root, filepath.FromSlash(m.relPath))

			// Step 1: path → URI (emit).
			uri, err := PathToURI(root, absPath)
			if err != nil {
				t.Fatalf("PathToURI(%q): %v", absPath, err)
			}
			if uri != m.wantURI {
				t.Errorf("PathToURI = %q; want %q", uri, m.wantURI)
			}

			// Step 2: URI → resolved path (parse).  This is the genuine round-trip
			// leg: ResolveURI must recover the original absolute path from the
			// emitted URI.
			res, err := ResolveURI(root, uri)
			if err != nil {
				t.Fatalf("ResolveURI(%q): %v", uri, err)
			}
			if res.Path != absPath {
				t.Errorf("round-trip path mismatch:\n  PathToURI input:   %q\n  ResolveURI output: %q", absPath, res.Path)
			}
		})
	}
}

// TestPathToURI_RoundTrip_FragmentPreservation is an explicit edge-case that
// the table-driven generator above does not naturally produce: a URI carrying
// a fragment must survive the round-trip with the fragment intact.
//
// This is kept as a separate named test (not a row in roundTripMappings) because
// fragment preservation is a URI-layer invariant, not a projection-mapping
// concern — it applies across every namespace equally and does not belong to any
// single mapping row.
func TestPathToURI_RoundTrip_FragmentPreservation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Construct a mem path and emit its URI, then manually append a fragment and
	// verify that ResolveURI preserves it.
	absPath := filepath.Join(root, ".cog/mem/semantic/insights/eigenform.cog.md")
	uri, err := PathToURI(root, absPath)
	if err != nil {
		t.Fatalf("PathToURI: %v", err)
	}

	uriWithFragment := uri + "#The-Seed"
	res, err := ResolveURI(root, uriWithFragment)
	if err != nil {
		t.Fatalf("ResolveURI(%q): %v", uriWithFragment, err)
	}
	if res.Path != absPath {
		t.Errorf("round-trip path mismatch:\n  input:  %q\n  output: %q", absPath, res.Path)
	}
	if res.Fragment != "The-Seed" {
		t.Errorf("Fragment = %q; want %q (fragment was dropped)", res.Fragment, "The-Seed")
	}
}

// ── 4. ErrUnknownAuthority ────────────────────────────────────────────────────

func TestResolveURI_UnknownAuthority(t *testing.T) {
	t.Parallel()
	root := "/workspace"

	// cog://workspace/... where "workspace" is not a known projection name
	// must return ErrUnknownAuthority (not a generic parse error).
	crossWorkspace := []string{
		"cog://my-other-workspace/mem/semantic/foo.cog.md",
		"cog://laptop/conf/kernel.yaml",
		"cog://staging-env/crystal",
	}

	for _, uri := range crossWorkspace {
		uri := uri
		t.Run(uri, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveURI(root, uri)
			if err == nil {
				t.Fatalf("ResolveURI(%q): expected ErrUnknownAuthority, got nil", uri)
			}
			if !errors.Is(err, ErrUnknownAuthority) {
				t.Errorf("ResolveURI(%q): got %v; want errors.Is ErrUnknownAuthority", uri, err)
			}
		})
	}
}

func TestResolveURI_KnownProjectionAsAuthorityStillWorks(t *testing.T) {
	t.Parallel()
	root := "/workspace"

	// A known projection name used as the authority component should NOT trigger
	// ErrUnknownAuthority — it should resolve normally.
	// e.g. cog://mem/semantic/foo.cog.md is still a valid form.
	_, err := ResolveURI(root, "cog://mem/semantic/foo.cog.md")
	if err != nil {
		t.Errorf("known projection in authority position: unexpected error %v", err)
	}
}

// ── 5. Fragment-before-query rejection (issue #171) ───────────────────────────

// TestResolveURI_FragmentBeforeQuery_Rejected verifies that a malformed URI
// with '#' appearing before '?' is rejected rather than silently bypassing the
// digest fail-closed check (ADR-067 §170).
//
// Without the fix: cog:adr/067#frag?digest=sha256:abc is parsed as
// fragment="frag?digest=sha256:abc", leaving no '?' in rest, so
// ErrDigestNotVerified is never returned.
func TestResolveURI_FragmentBeforeQuery_Rejected(t *testing.T) {
	t.Parallel()
	root := "/workspace"

	malformed := []string{
		"cog:adr/067#frag?digest=sha256:abc",
		"cog:mem/semantic/foo.cog.md#Section?digest=sha256:deadbeef",
		"cog://mem/semantic/foo.cog.md#Anchor?ref=main",
	}

	for _, uri := range malformed {
		uri := uri
		t.Run(uri, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveURI(root, uri)
			if err == nil {
				t.Fatalf("ResolveURI(%q): expected error for malformed fragment-before-query URI, got nil", uri)
			}
		})
	}
}

// TestResolveURI_DigestWithFragment_WellFormed verifies that a well-formed URI
// carrying both a query and a fragment in RFC 3986 order (?query#fragment) is
// still correctly fail-closed on a digest param.
func TestResolveURI_DigestWithFragment_WellFormed(t *testing.T) {
	t.Parallel()
	root := "/workspace"

	// Well-formed: ?query#fragment — digest must still fail-closed.
	uri := "cog:mem/semantic/foo.cog.md?digest=sha256:abc#Section"
	_, err := ResolveURI(root, uri)
	if err == nil {
		t.Fatalf("ResolveURI(%q): expected ErrDigestNotVerified, got nil", uri)
	}
	if !errors.Is(err, ErrDigestNotVerified) {
		t.Errorf("ResolveURI(%q): got %v; want errors.Is ErrDigestNotVerified", uri, err)
	}
}

// ── 6. Fragment preserved on well-formed ?query#fragment URI (PR #176 regression) ─

// TestResolveURI_FragmentPreservedWithQuery verifies that a well-formed URI
// carrying both a non-digest query param and a fragment in RFC 3986 order
// (?query#fragment) correctly preserves the fragment in the returned
// URIResolution.
//
// PR #176 introduced a regression: rest was truncated at '?' before the
// fragment-extraction pass, so the fragment was silently dropped.
// Concretely: cog:adr/074?ref=main#section-2 would return Fragment="" instead
// of Fragment="section-2".
func TestResolveURI_FragmentPreservedWithQuery(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// adr uses glob pattern — create the file so ResolveURI can find it.
	if err := os.MkdirAll(filepath.Join(root, ".cog", "adr"), 0755); err != nil {
		t.Fatal(err)
	}
	adrFile := filepath.Join(root, ".cog", "adr", "074-some-decision.md")
	if err := os.WriteFile(adrFile, []byte("# ADR-074\n"), 0644); err != nil {
		t.Fatal(err)
	}

	uri := "cog:adr/074?ref=main#section-2"
	res, err := ResolveURI(root, uri)
	if err != nil {
		t.Fatalf("ResolveURI(%q): unexpected error %v", uri, err)
	}
	if res.Fragment != "section-2" {
		t.Errorf("Fragment = %q; want %q (fragment was dropped — PR #176 regression)", res.Fragment, "section-2")
	}
}
