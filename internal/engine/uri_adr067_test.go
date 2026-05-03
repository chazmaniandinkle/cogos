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

// TestPathToURI_RoundTrip verifies parse → emit → parse round-trip equivalence
// for projection URIs (ADR-067).
//
// The claim in this file's header ("parse → emit → parse gives identical
// resolution") requires calling ResolveURI on the emitted URI and comparing
// the resolved path back to the original absolute path.  The previous
// implementation called PathToURI twice without ever calling ResolveURI,
// making it a path-only stability test rather than a genuine round-trip.
//
// Covers both local projection form (cog:projection/path) and confirms
// bare-form URIs resolve to the expected filesystem paths.
func TestPathToURI_RoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Set up a minimal workspace structure so ResolveURI can construct paths.
	// "direct" and "singleton" projections do not require files to exist; they
	// compute the path deterministically.
	for _, dir := range []string{
		".cog/mem/semantic/insights",
		".cog/config",
		".cog/ontology",
		".cog/docs",
	} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name    string
		absPath string
		wantURI string
	}{
		{
			name:    "mem bare form",
			absPath: filepath.Join(root, ".cog/mem/semantic/insights/foo.cog.md"),
			wantURI: "cog:mem/semantic/insights/foo.cog.md",
		},
		{
			name:    "conf bare form",
			absPath: filepath.Join(root, ".cog/config/kernel.yaml"),
			wantURI: "cog:conf/kernel.yaml",
		},
		{
			// ontology strips .cog.md from the URI; ResolveURI re-adds it.
			name:    "ontology extension-stripped",
			absPath: filepath.Join(root, ".cog/ontology/crystal.cog.md"),
			wantURI: "cog:ontology/crystal",
		},
		{
			name:    "docs bare form",
			absPath: filepath.Join(root, ".cog/docs/framework-status.md"),
			wantURI: "cog:docs/framework-status.md",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Step 1: path → URI (emit).
			uri, err := PathToURI(root, tc.absPath)
			if err != nil {
				t.Fatalf("PathToURI(%q): %v", tc.absPath, err)
			}
			if uri != tc.wantURI {
				t.Errorf("PathToURI = %q; want %q", uri, tc.wantURI)
			}

			// Step 2: URI → resolved path (parse).  This is the round-trip leg
			// the previous test was missing: call ResolveURI on the emitted URI
			// and verify the resolved filesystem path matches the original input.
			res, err := ResolveURI(root, uri)
			if err != nil {
				t.Fatalf("ResolveURI(%q): %v", uri, err)
			}
			if res.Path != tc.absPath {
				t.Errorf("round-trip path mismatch:\n  PathToURI input:   %q\n  ResolveURI output: %q", tc.absPath, res.Path)
			}
		})
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
