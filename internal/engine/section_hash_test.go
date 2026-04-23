package engine

import (
	"strings"
	"testing"
)

func TestGenerateSectionIndex_EmptyInput(t *testing.T) {
	got := GenerateSectionIndex("")
	if got == nil {
		t.Fatalf("GenerateSectionIndex(\"\") returned nil; want empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("GenerateSectionIndex(\"\") = %v; want empty slice", got)
	}
}

func TestGenerateSectionIndex_NoHeadings(t *testing.T) {
	md := "Just a paragraph.\nAnd another line.\n"
	got := GenerateSectionIndex(md)
	if len(got) != 0 {
		t.Fatalf("GenerateSectionIndex with no h2 headings = %v; want empty slice", got)
	}
}

func TestGenerateSectionIndex_SingleSection(t *testing.T) {
	md := "## Introduction\n\nHello world.\n"
	got := GenerateSectionIndex(md)
	if len(got) != 1 {
		t.Fatalf("GenerateSectionIndex single section: got %d sections, want 1", len(got))
	}
	s := got[0]
	if s.Title != "Introduction" {
		t.Errorf("Title = %q; want %q", s.Title, "Introduction")
	}
	if s.Anchor != "introduction" {
		t.Errorf("Anchor = %q; want %q", s.Anchor, "introduction")
	}
	if !strings.HasPrefix(s.Hash, "sha256:") {
		t.Errorf("Hash = %q; want sha256:<hex> prefix", s.Hash)
	}
	// sha256 hex is 64 chars; plus prefix "sha256:" = 71.
	if len(s.Hash) != 71 {
		t.Errorf("Hash length = %d; want 71 (sha256: + 64 hex)", len(s.Hash))
	}
	if s.Size <= 0 {
		t.Errorf("Size = %d; want > 0", s.Size)
	}
}

func TestGenerateSectionIndex_MultipleSections(t *testing.T) {
	md := `## First
body1

## Second
body2

## Third
body3
`
	got := GenerateSectionIndex(md)
	if len(got) != 3 {
		t.Fatalf("got %d sections, want 3", len(got))
	}
	wantTitles := []string{"First", "Second", "Third"}
	wantAnchors := []string{"first", "second", "third"}
	seen := map[string]bool{}
	for i, s := range got {
		if s.Title != wantTitles[i] {
			t.Errorf("sections[%d].Title = %q; want %q", i, s.Title, wantTitles[i])
		}
		if s.Anchor != wantAnchors[i] {
			t.Errorf("sections[%d].Anchor = %q; want %q", i, s.Anchor, wantAnchors[i])
		}
		if seen[s.Hash] {
			t.Errorf("duplicate hash across distinct sections: %s", s.Hash)
		}
		seen[s.Hash] = true
	}
}

func TestGenerateSectionIndex_Determinism(t *testing.T) {
	md := "## Alpha\ncontent A\n\n## Beta\ncontent B\n"
	a := GenerateSectionIndex(md)
	b := GenerateSectionIndex(md)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Hash != b[i].Hash {
			t.Errorf("non-deterministic hash at index %d: %s vs %s", i, a[i].Hash, b[i].Hash)
		}
	}

	// Identical section content across two different parent documents
	// should yield identical hashes for that section.
	md2 := "## Alpha\ncontent A\n\n## Gamma\ndifferent tail\n"
	c := GenerateSectionIndex(md2)
	if len(c) < 1 {
		t.Fatalf("expected at least one section in md2")
	}
	if a[0].Hash != c[0].Hash {
		t.Errorf("identical 'Alpha' content across docs produced different hashes: %s vs %s", a[0].Hash, c[0].Hash)
	}
}

func TestGenerateSectionIndex_ContentSensitivity(t *testing.T) {
	mdA := "## Section\nhello\n"
	mdB := "## Section\nhellx\n" // one-byte change
	a := GenerateSectionIndex(mdA)
	b := GenerateSectionIndex(mdB)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected exactly one section each; got %d and %d", len(a), len(b))
	}
	if a[0].Hash == b[0].Hash {
		t.Errorf("content change did not alter hash: both = %s", a[0].Hash)
	}
}

func TestGenerateSectionIndex_TrailingWhitespaceStable(t *testing.T) {
	base := "## Stable\nbody line\n"
	withTrailingSpaces := "## Stable   \nbody line\t\t\n"
	withTrailingBlankLines := "## Stable\nbody line\n\n\n\n"
	withAllOfTheAbove := "## Stable  \nbody line \n\n  \n\t\n"

	want := GenerateSectionIndex(base)
	if len(want) != 1 {
		t.Fatalf("base: got %d sections, want 1", len(want))
	}
	for _, variant := range []string{withTrailingSpaces, withTrailingBlankLines, withAllOfTheAbove} {
		got := GenerateSectionIndex(variant)
		if len(got) != 1 {
			t.Fatalf("variant %q: got %d sections, want 1", variant, len(got))
		}
		if got[0].Hash != want[0].Hash {
			t.Errorf("trailing-whitespace variant changed hash\n  base: %s\n  variant (%q): %s",
				want[0].Hash, variant, got[0].Hash)
		}
		if got[0].Size != want[0].Size {
			t.Errorf("trailing-whitespace variant changed Size\n  base: %d\n  variant (%q): %d",
				want[0].Size, variant, got[0].Size)
		}
	}
}

func TestGenerateSectionIndex_SlugifyEdgeCases(t *testing.T) {
	cases := []struct {
		md     string
		anchor string
	}{
		{"## Hello World\nx\n", "hello-world"},
		{"## Foo: Bar!\nx\n", "foo-bar"},
		{"## --Leading and trailing--\nx\n", "leading-and-trailing"},
		{"## Mixed CASE 123\nx\n", "mixed-case-123"},
		{"## Multiple   Spaces\nx\n", "multiple-spaces"},
	}
	for _, c := range cases {
		got := GenerateSectionIndex(c.md)
		if len(got) != 1 {
			t.Fatalf("case %q: got %d sections, want 1", c.md, len(got))
		}
		if got[0].Anchor != c.anchor {
			t.Errorf("case %q: Anchor = %q; want %q", c.md, got[0].Anchor, c.anchor)
		}
	}
}

func TestGenerateSectionIndex_PrefaceDiscarded(t *testing.T) {
	md := `# Document Title

Some preamble paragraph that is not under any h2.

## Real First
body
`
	got := GenerateSectionIndex(md)
	if len(got) != 1 {
		t.Fatalf("got %d sections, want 1", len(got))
	}
	if got[0].Title != "Real First" {
		t.Errorf("Title = %q; want %q", got[0].Title, "Real First")
	}
}

func TestGenerateSectionIndex_H3IsNotASection(t *testing.T) {
	md := `## Parent
intro

### Child heading
child body

## Sibling
sibling body
`
	got := GenerateSectionIndex(md)
	if len(got) != 2 {
		t.Fatalf("got %d sections, want 2 (h3 should not split)", len(got))
	}
	if got[0].Title != "Parent" || got[1].Title != "Sibling" {
		t.Errorf("titles = [%q, %q]; want [Parent, Sibling]", got[0].Title, got[1].Title)
	}
	// Child heading text should be part of the Parent section's body,
	// so Parent's hash should differ from a variant without the h3.
	without := GenerateSectionIndex("## Parent\nintro\n\n## Sibling\nsibling body\n")
	if len(without) != 2 {
		t.Fatalf("without-h3 variant: got %d sections, want 2", len(without))
	}
	if got[0].Hash == without[0].Hash {
		t.Errorf("Parent hash unchanged despite different body content: %s", got[0].Hash)
	}
}

func TestComputeSectionHash_KnownValue(t *testing.T) {
	// Stability anchor: sha256("") in the prefixed form.
	got := ComputeSectionHash([]byte(""))
	const want = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("ComputeSectionHash(\"\") = %s; want %s", got, want)
	}
}
