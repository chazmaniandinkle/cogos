package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// ComputeSectionHash returns the canonical content hash for a Section's
// content bytes in the form "sha256:<hex>". The caller is responsible for
// passing the already-canonicalized content bytes; ComputeSectionHash does
// not re-canonicalize (RFC 8785-style canonicalization applies at the
// parse step in GenerateSectionIndex).
func ComputeSectionHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// GenerateSectionIndex parses a Markdown document into h2-level sections
// and returns a []Section with a stable content hash for each.
//
// A section spans from one `## Heading` line up to (but not including) the
// next `## Heading` line. Any content preceding the first h2 heading is
// discarded — only content addressable under an h2 title is indexed.
//
// The content used for hashing is the section's raw bytes (heading line
// plus body) with trailing whitespace on each line trimmed and trailing
// blank lines removed, so that cosmetic whitespace edits do not change
// the hash. This is a lightweight canonicalization in the spirit of
// ADR-059 Phase 2 / RFC 8785.
//
// Anchor is the slug form of the title: lowercase, ASCII alphanumerics
// and hyphens only, spaces collapsed to single hyphens, leading/trailing
// hyphens stripped. This matches common Markdown anchor conventions.
//
// Returns an empty []Section for empty or heading-free input.
func GenerateSectionIndex(markdown string) []Section {
	if markdown == "" {
		return []Section{}
	}

	lines := strings.Split(markdown, "\n")
	type rawSection struct {
		title string
		lines []string // includes the heading line
	}
	var raw []rawSection
	var cur *rawSection

	for _, line := range lines {
		if isH2Heading(line) {
			if cur != nil {
				raw = append(raw, *cur)
			}
			title := extractH2Title(line)
			cur = &rawSection{
				title: title,
				lines: []string{line},
			}
			continue
		}
		if cur != nil {
			cur.lines = append(cur.lines, line)
		}
		// Lines before the first h2 are ignored.
	}
	if cur != nil {
		raw = append(raw, *cur)
	}

	if len(raw) == 0 {
		return []Section{}
	}

	out := make([]Section, 0, len(raw))
	for _, r := range raw {
		canon := canonicalizeSection(r.lines)
		contentBytes := []byte(canon)
		out = append(out, Section{
			Title:  r.title,
			Anchor: slugifyAnchor(r.title),
			Hash:   ComputeSectionHash(contentBytes),
			Size:   len(contentBytes),
		})
	}
	return out
}

// isH2Heading reports whether a line is an ATX-style h2 heading
// (exactly two leading '#' followed by a space). Leading whitespace is
// allowed per CommonMark-ish tolerance but the heading marker itself
// must be `## `.
func isH2Heading(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(trimmed, "## ") {
		return false
	}
	// Reject h3+ (`### `, `#### `, etc.) which also pass the prefix check
	// above only if the prefix is literally "## ". `strings.HasPrefix` on
	// "## " already rejects "### " because "### "[0:3] == "###" != "## ".
	// So no extra check needed. But we do want to reject lines like
	// "##\t" or "##foo" — the HasPrefix on "## " handles that too.
	return true
}

// extractH2Title returns the heading text from an h2 line, stripping
// the `## ` prefix and any trailing whitespace or trailing `#` run
// (CommonMark allows `## Foo ##` as an alternate heading form).
func extractH2Title(line string) string {
	trimmed := strings.TrimLeft(line, " \t")
	trimmed = strings.TrimPrefix(trimmed, "## ")
	trimmed = strings.TrimRight(trimmed, " \t")
	// Strip optional trailing run of '#' and then more whitespace.
	trimmed = strings.TrimRight(trimmed, "#")
	trimmed = strings.TrimRight(trimmed, " \t")
	return trimmed
}

// canonicalizeSection joins section lines with "\n" after trimming
// trailing whitespace from each line and dropping trailing blank lines.
// This is intentionally conservative: it removes cosmetic variance
// (trailing spaces, extra blank lines at end) while preserving all
// semantically meaningful content and ordering.
func canonicalizeSection(lines []string) string {
	cleaned := make([]string, len(lines))
	for i, l := range lines {
		cleaned[i] = strings.TrimRightFunc(l, unicode.IsSpace)
	}
	// Drop trailing blank lines.
	end := len(cleaned)
	for end > 0 && cleaned[end-1] == "" {
		end--
	}
	return strings.Join(cleaned[:end], "\n")
}

// slugifyAnchor converts a heading title into a URL-safe anchor slug.
// ASCII letters are lowercased; digits are preserved; everything else
// becomes a hyphen separator. Runs of hyphens collapse to one, and
// leading / trailing hyphens are trimmed. Distinct from the package
// helper `slugify` (mcp_server.go) which additionally truncates to 50
// chars — we avoid truncation here to keep section anchors injective
// with respect to their full titles.
func slugifyAnchor(title string) string {
	var b strings.Builder
	b.Grow(len(title))
	prevHyphen := true // suppress leading hyphens
	for _, r := range title {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevHyphen = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	out := b.String()
	return strings.TrimRight(out, "-")
}
