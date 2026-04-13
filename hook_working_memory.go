// hook_working_memory.go
// Kernel-owned working memory lifecycle for CogOS v2.4.0
//
// Manages per-session working memory creation, update, and sealing.
// Replaces Python hooks: pre-inference creation, post-inference update, session-end seal.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// === Constants ===

const (
	wmFilename        = "working.cog.md"
	wmDefaultSession  = "_default"
	wmMaxSectionItems = 10
	wmMaxNewDecisions = 3
	wmMaxNewArtifacts = 5
	wmMaxNewQuestions = 3
)

// Transient request prefixes that map to the default session.
var wmTransientPrefixes = []string{"req-http-", "req-cli-"}

// === Path Resolution ===

// workingMemoryPath returns the filesystem path for a session's working memory file.
func workingMemoryPath(workspaceRoot, sessionID string) string {
	sid := resolveWMSessionID(sessionID)
	return filepath.Join(workspaceRoot, ".cog", "mem", "episodic", "sessions", sid, wmFilename)
}

// resolveWMSessionID normalizes a raw session ID.
// Empty, "unknown", or transient IDs map to the default session.
func resolveWMSessionID(raw string) string {
	if raw == "" || raw == "unknown" {
		return wmDefaultSession
	}
	for _, prefix := range wmTransientPrefixes {
		if strings.HasPrefix(raw, prefix) {
			return wmDefaultSession
		}
	}
	return raw
}

// === Frontmatter Helpers ===

// parseFrontmatter splits a document into frontmatter key-value pairs and body.
// Frontmatter is the YAML between opening and closing "---" lines.
// Returns ordered keys (to preserve field order), a map of values, and the body.
func parseFrontmatter(content string) ([]string, map[string]string, string) {
	fm := make(map[string]string)
	var keys []string

	if !strings.HasPrefix(content, "---\n") {
		return keys, fm, content
	}

	endIdx := strings.Index(content[4:], "\n---")
	if endIdx == -1 {
		return keys, fm, content
	}

	fmBlock := content[4 : 4+endIdx]
	body := content[4+endIdx+4:] // skip past "\n---"
	if strings.HasPrefix(body, "\n") {
		body = body[1:]
	}

	for _, line := range strings.Split(fmBlock, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}
		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		keys = append(keys, key)
		fm[key] = value
	}

	return keys, fm, body
}

// updateFrontmatter sets a key in the frontmatter map and tracks key order.
// If the key already exists, it updates the value; otherwise it appends.
func updateFrontmatter(keys []string, fm map[string]string, key, value string) []string {
	fm[key] = value
	for _, k := range keys {
		if k == key {
			return keys
		}
	}
	return append(keys, key)
}

// serializeFrontmatter reconstructs a document from frontmatter and body.
func serializeFrontmatter(keys []string, fm map[string]string, body string) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	for _, key := range keys {
		if val, ok := fm[key]; ok {
			sb.WriteString(key)
			sb.WriteString(": ")
			sb.WriteString(val)
			sb.WriteString("\n")
		}
	}
	sb.WriteString("---\n")
	if body != "" {
		sb.WriteString("\n")
		sb.WriteString(body)
	}
	return sb.String()
}

// === Section Parsing ===

// wmSections represents the parsed sections of a working memory document.
type wmSections struct {
	CurrentFocus string
	Artifacts    string
	Questions    string
	Decisions    string
	NextActions  string
}

// parseWMSections extracts section content from the body (after frontmatter).
func parseWMSections(body string) wmSections {
	var s wmSections

	sectionRe := regexp.MustCompile(`(?m)^# (.+?)$`)
	matches := sectionRe.FindAllStringIndex(body, -1)

	for i, loc := range matches {
		headerEnd := loc[1]
		header := body[loc[0]:headerEnd]

		// Find content between this header and the next (or end)
		contentStart := headerEnd
		var contentEnd int
		if i+1 < len(matches) {
			contentEnd = matches[i+1][0]
		} else {
			contentEnd = len(body)
		}
		content := strings.TrimSpace(body[contentStart:contentEnd])

		headerLower := strings.ToLower(header)
		switch {
		case strings.Contains(headerLower, "focus"):
			s.CurrentFocus = content
		case strings.Contains(headerLower, "artifact"):
			s.Artifacts = content
		case strings.Contains(headerLower, "question"):
			s.Questions = content
		case strings.Contains(headerLower, "decision"):
			s.Decisions = content
		case strings.Contains(headerLower, "action") || strings.Contains(headerLower, "next"):
			s.NextActions = content
		}
	}

	return s
}

// rebuildBody reconstructs the markdown body from sections.
func rebuildBody(s wmSections) string {
	focus := s.CurrentFocus
	if focus == "" || focus == "(none yet)" {
		focus = "(none yet)"
	}
	artifacts := s.Artifacts
	if artifacts == "" || artifacts == "(none yet)" {
		artifacts = "(none yet)"
	}
	questions := s.Questions
	if questions == "" || questions == "(none yet)" {
		questions = "(none yet)"
	}
	decisions := s.Decisions
	if decisions == "" || decisions == "(none yet)" {
		decisions = "(none yet)"
	}
	actions := s.NextActions
	if actions == "" || actions == "(none yet)" {
		actions = "(none yet)"
	}

	return fmt.Sprintf("# Current Focus\n%s\n\n# Active Artifacts\n%s\n\n# Open Questions\n%s\n\n# Key Decisions This Session\n%s\n\n# Next Actions\n%s\n",
		focus, artifacts, questions, decisions, actions)
}

// === Extraction from Response Text ===

var (
	decisionPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:^|\n)\s*Decision:\s*(.+)`),
		regexp.MustCompile(`(?i)(?:^|\n).*?\b(?:decided|chose|going with|will use)\b[:\s]+(.+?)(?:\.|$)`),
	}

	artifactCogURIRe = regexp.MustCompile(`cog://\S+`)
	artifactPathRe   = regexp.MustCompile(`(?:^|[\s\x60"'(])(/[a-zA-Z0-9_./-]+\.(?:go|py|js|ts|md|yaml|yml|json|toml|sh|rs|c|h|css|html))`)
	artifactWriteRe  = regexp.MustCompile(`(?i)(?:wrote|created|updated).*?[\x60"']([^\x60"']+\.(?:go|py|js|ts|md|yaml|yml|json|toml|sh))[\x60"']`)

	questionLineRe = regexp.MustCompile(`(?m)^.+\?\s*$`)
	todoQuestionRe = regexp.MustCompile(`(?i)(?:TODO|need to (?:figure out|decide|determine)):\s*(.+?)(?:\n|$)`)
)

// extractDecisions extracts decision-like statements from response text.
func extractDecisions(response string) []string {
	var decisions []string
	seen := make(map[string]bool)

	for _, re := range decisionPatterns {
		for _, match := range re.FindAllStringSubmatch(response, -1) {
			if len(match) < 2 {
				continue
			}
			d := strings.TrimSpace(match[1])
			if len(d) < 20 || len(d) > 200 {
				continue
			}
			if seen[d] {
				continue
			}
			seen[d] = true
			decisions = append(decisions, d)
			if len(decisions) >= wmMaxNewDecisions {
				return decisions
			}
		}
	}
	return decisions
}

// extractArtifacts extracts file path and URI references from response text.
func extractArtifacts(response string) []string {
	seen := make(map[string]bool)
	var artifacts []string

	addArtifact := func(a string) {
		if !seen[a] {
			seen[a] = true
			artifacts = append(artifacts, a)
		}
	}

	// cog:// URIs
	for _, match := range artifactCogURIRe.FindAllString(response, -1) {
		addArtifact(match)
	}

	// Paths from wrote/created/updated context
	for _, match := range artifactWriteRe.FindAllStringSubmatch(response, -1) {
		if len(match) >= 2 {
			addArtifact(match[1])
		}
	}

	// Absolute file paths
	for _, match := range artifactPathRe.FindAllStringSubmatch(response, -1) {
		if len(match) >= 2 {
			addArtifact(match[1])
		}
	}

	if len(artifacts) > wmMaxNewArtifacts {
		artifacts = artifacts[:wmMaxNewArtifacts]
	}
	return artifacts
}

// extractQuestions extracts open questions from response text.
func extractQuestions(response string) []string {
	var questions []string
	seen := make(map[string]bool)

	// Lines ending with ?
	for _, match := range questionLineRe.FindAllString(response, -1) {
		q := strings.TrimSpace(match)
		if len(q) < 10 {
			continue
		}
		// Skip markdown headers used as rhetorical questions
		if strings.HasPrefix(q, "#") {
			continue
		}
		if seen[q] {
			continue
		}
		seen[q] = true
		questions = append(questions, q)
		if len(questions) >= wmMaxNewQuestions {
			return questions
		}
	}

	// TODO/need-to patterns
	for _, match := range todoQuestionRe.FindAllStringSubmatch(response, -1) {
		if len(match) < 2 {
			continue
		}
		q := "[ ] " + strings.TrimSpace(match[1])
		if seen[q] {
			continue
		}
		seen[q] = true
		questions = append(questions, q)
		if len(questions) >= wmMaxNewQuestions {
			return questions
		}
	}

	return questions
}

// === Section Item Management ===

// appendToSection adds new bullet items to a section, capping at maxItems.
// Existing items are preserved; duplicates are skipped.
func appendToSection(existing string, newItems []string, maxItems int) string {
	if len(newItems) == 0 {
		return existing
	}

	// Parse existing items
	var lines []string
	for _, line := range strings.Split(existing, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && trimmed != "(none yet)" && trimmed != "(None yet)" {
			lines = append(lines, line)
		}
	}

	// Append new items, skipping duplicates
	for _, item := range newItems {
		bullet := "- " + item
		duplicate := false
		for _, existing := range lines {
			if strings.Contains(existing, item) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			lines = append(lines, bullet)
		}
	}

	// Cap at maxItems (drop oldest)
	if len(lines) > maxItems {
		lines = lines[len(lines)-maxItems:]
	}

	return strings.Join(lines, "\n")
}

// === Atomic File Write ===

// atomicWriteFile writes content to a file atomically using a temp file + rename.
func atomicWriteFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	tmpFile, err := os.CreateTemp(dir, ".wm-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	_, writeErr := tmpFile.WriteString(content)
	closeErr := tmpFile.Close()

	if writeErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", writeErr)
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", closeErr)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}

// === Public API ===

// nowUTC returns the current UTC time formatted as ISO 8601.
func nowUTC() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05")
}

// workingMemoryTemplate returns a fresh working memory document.
func workingMemoryTemplate(sessionID string) string {
	sid := resolveWMSessionID(sessionID)
	now := nowUTC()
	return fmt.Sprintf(`---
type: working-memory
session: %s
created: %s
updated: %s
turn: 0
sealed: false
---

# Current Focus
(none yet)

# Active Artifacts
(none yet)

# Open Questions
(none yet)

# Key Decisions This Session
(none yet)

# Next Actions
(none yet)
`, sid, now, now)
}

// CreateWorkingMemory creates a fresh working memory file for a session.
// If the file already exists, this is a no-op (idempotent).
func CreateWorkingMemory(workspaceRoot, sessionID string) error {
	wmPath := workingMemoryPath(workspaceRoot, sessionID)

	// Idempotent: skip if file already exists
	if _, err := os.Stat(wmPath); err == nil {
		return nil
	}

	content := workingMemoryTemplate(sessionID)
	return atomicWriteFile(wmPath, content)
}

// UpdateWorkingMemory updates a session's working memory after an inference response.
// Extracts decisions, artifacts, and questions from the response text.
// If the file does not exist, it is created first.
func UpdateWorkingMemory(workspaceRoot, sessionID, responseText string) error {
	wmPath := workingMemoryPath(workspaceRoot, sessionID)

	// Read existing content, or create if missing
	data, err := os.ReadFile(wmPath)
	if err != nil {
		if os.IsNotExist(err) {
			if createErr := CreateWorkingMemory(workspaceRoot, sessionID); createErr != nil {
				return fmt.Errorf("auto-create working memory: %w", createErr)
			}
			data, err = os.ReadFile(wmPath)
			if err != nil {
				return fmt.Errorf("read after create: %w", err)
			}
		} else {
			return fmt.Errorf("read working memory: %w", err)
		}
	}

	content := string(data)

	// Parse existing document
	keys, fm, body := parseFrontmatter(content)
	sections := parseWMSections(body)

	// Extract new items from response
	newDecisions := extractDecisions(responseText)
	newArtifacts := extractArtifacts(responseText)
	newQuestions := extractQuestions(responseText)

	// Update sections
	sections.Decisions = appendToSection(sections.Decisions, newDecisions, wmMaxSectionItems)
	sections.Artifacts = appendToSection(sections.Artifacts, newArtifacts, wmMaxSectionItems)
	sections.Questions = appendToSection(sections.Questions, newQuestions, wmMaxSectionItems)

	// Update frontmatter
	keys = updateFrontmatter(keys, fm, "updated", nowUTC())

	turnStr := fm["turn"]
	turn, _ := strconv.Atoi(turnStr)
	turn++
	keys = updateFrontmatter(keys, fm, "turn", strconv.Itoa(turn))

	// Rebuild and write
	newBody := rebuildBody(sections)
	newContent := serializeFrontmatter(keys, fm, newBody)

	return atomicWriteFile(wmPath, newContent)
}

// SealWorkingMemory marks a session's working memory as sealed.
// Adds sealed: true and sealed_at timestamp to frontmatter.
// If the file does not exist or is already sealed, this is a no-op.
func SealWorkingMemory(workspaceRoot, sessionID string) error {
	wmPath := workingMemoryPath(workspaceRoot, sessionID)

	data, err := os.ReadFile(wmPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Nothing to seal
		}
		return fmt.Errorf("read working memory: %w", err)
	}

	content := string(data)

	// Already sealed? No-op.
	if strings.Contains(content, "sealed: true") {
		return nil
	}

	keys, fm, body := parseFrontmatter(content)

	keys = updateFrontmatter(keys, fm, "sealed", "true")
	keys = updateFrontmatter(keys, fm, "sealed_at", nowUTC())

	newContent := serializeFrontmatter(keys, fm, body)
	return atomicWriteFile(wmPath, newContent)
}
