// Package skills provides shared skill discovery, frontmatter parsing, tier
// classification, and execution logic used by both the CLI (cmd_skill.go) and
// the HTTP handler (internal/engine/serve_skills.go).
//
// The package is stateless: all functions accept the skill directories (or
// workspace root) as parameters so callers control workspace resolution and
// there are no global side-effects.
package skills

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ─── Types ───────────────────────────────────────────────────────────────────

// Frontmatter holds the YAML front-matter fields parsed from a SKILL.md file.
// The front-matter block is optional; callers receive a zero-value struct when
// no front-matter is present.
type Frontmatter struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Exec        string   `yaml:"exec"`
	Schema      string   `yaml:"schema"`
	Requires    []string `yaml:"requires"`
	Timeout     string   `yaml:"timeout"` // e.g. "5m"
}

// Record is the canonical skill descriptor used by both surfaces.  Its JSON
// shape is the contract returned by `cog skill list --json` and
// GET /v1/skills.
type Record struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Tier        int      `json:"tier"`
	Description string   `json:"description"`
	Exec        *string  `json:"exec"`            // null for tier-0/1
	Schema      *string  `json:"schema"`          // null unless tier-3
	Path        string   `json:"path"`
	Requires    []string `json:"requires"`
	Gated       bool     `json:"gated,omitempty"` // true when capability-filtered
}

// ExecResult is the structured outcome of running a skill.
type ExecResult struct {
	Name       string `json:"name"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// ─── Frontmatter parsing ─────────────────────────────────────────────────────

// ParseFrontmatter extracts YAML front-matter from raw SKILL.md bytes. It also
// pulls a title and description from the first Markdown heading and leading
// paragraph, used as fallbacks when the YAML block is absent or incomplete.
//
// Returns: (frontmatter, title, firstParagraph).
func ParseFrontmatter(data []byte) (Frontmatter, string, string) {
	content := string(data)
	var fm Frontmatter

	// Extract YAML block between leading --- delimiters.
	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---")
		if end != -1 {
			_ = yaml.Unmarshal([]byte(content[4:4+end]), &fm)
			content = content[4+end+4:] // skip past closing ---\n
		}
	}

	// Extract title from the first Markdown # heading and description from
	// the first non-empty, non-heading, non-blockquote line.
	title := ""
	desc := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") && title == "" {
			title = strings.TrimPrefix(line, "# ")
			continue
		}
		if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, ">") && desc == "" {
			desc = line
			if len(desc) > 200 {
				desc = desc[:197] + "..."
			}
		}
	}

	return fm, title, desc
}

// ─── Tier classification ─────────────────────────────────────────────────────

// InferTier determines the tier of a skill from its directory layout and
// parsed front-matter.
//
//   - Tier 0: prose-only SKILL.md (default)
//   - Tier 1: helper scripts present but no declared exec
//   - Tier 2: exec field set in frontmatter and bin/<exec> exists
//   - Tier 3: tier-2 + schema field set
func InferTier(skillPath string, fm Frontmatter) int {
	if fm.Schema != "" && fm.Exec != "" {
		return 3
	}

	if fm.Exec != "" {
		binPath := filepath.Join(skillPath, fm.Exec)
		if _, err := os.Stat(binPath); err == nil {
			return 2
		}
	}

	// Check for any scripts at the skill root that make this tier-1.
	entries, err := os.ReadDir(skillPath)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".sh") || strings.HasSuffix(name, ".py") {
			return 1
		}
	}
	// bin/ exists with files but exec not declared — tier 1.
	if binEntries, err := os.ReadDir(filepath.Join(skillPath, "bin")); err == nil && len(binEntries) > 0 && fm.Exec == "" {
		return 1
	}

	return 0
}

// ─── Record loading ──────────────────────────────────────────────────────────

// LoadRecord reads a skill directory and returns a populated Record, or an
// error if the directory does not contain a valid SKILL.md.
func LoadRecord(name, skillPath string) (*Record, error) {
	data, err := os.ReadFile(filepath.Join(skillPath, "SKILL.md"))
	if err != nil {
		return nil, fmt.Errorf("read SKILL.md: %w", err)
	}

	fm, title, desc := ParseFrontmatter(data)

	// Name: frontmatter wins; fall back to directory name.
	skillName := name
	if fm.Name != "" {
		skillName = fm.Name
	}

	// Description: frontmatter wins; fall back to Markdown extraction.
	skillDesc := fm.Description
	if skillDesc == "" {
		skillDesc = desc
	}
	if skillDesc == "" && title != "" {
		skillDesc = title
	}

	version := fm.Version
	if version == "" {
		version = "0.1.0"
	}

	tier := InferTier(skillPath, fm)

	rec := &Record{
		Name:        skillName,
		Version:     version,
		Tier:        tier,
		Description: skillDesc,
		Path:        skillPath,
		Requires:    fm.Requires,
	}
	if rec.Requires == nil {
		rec.Requires = []string{}
	}
	if fm.Exec != "" {
		e := fm.Exec
		rec.Exec = &e
	}
	if fm.Schema != "" {
		s := fm.Schema
		rec.Schema = &s
	}

	return rec, nil
}

// ─── Discovery ───────────────────────────────────────────────────────────────

// Discover walks dirs in order and returns a deduplicated list of Records.
// Later directories in the slice override earlier ones for skills with the
// same name (workspace-level skills win over user-level skills when the caller
// passes user-level dirs first).
//
// Non-existent directories are silently skipped; other I/O errors are returned.
func Discover(dirs []string) ([]Record, error) {
	seen := make(map[string]Record)
	var order []string // preserve stable insertion order

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read skills dir %s: %w", dir, err)
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			rec, err := LoadRecord(name, filepath.Join(dir, name))
			if err != nil {
				// Not a valid skill directory; skip silently.
				continue
			}

			if _, exists := seen[rec.Name]; !exists {
				order = append(order, rec.Name)
			}
			seen[rec.Name] = *rec
		}
	}

	result := make([]Record, 0, len(order))
	for _, name := range order {
		result = append(result, seen[name])
	}
	return result, nil
}

// FindByName looks up a single skill by name across dirs.
// Returns an error when the skill is not found.
func FindByName(name string, dirs []string) (*Record, error) {
	skills, err := Discover(dirs)
	if err != nil {
		return nil, err
	}
	for i := range skills {
		if skills[i].Name == name {
			return &skills[i], nil
		}
	}
	return nil, fmt.Errorf("skill not found: %s", name)
}

// ─── Execution ───────────────────────────────────────────────────────────────

// Exec runs a tier-2 or tier-3 skill and returns the structured result.
//
// Parameters:
//   - ctx: parent context; Exec derives a child context with the resolved timeout.
//   - skill: the Record to execute.
//   - inputJSON: raw JSON payload forwarded to the skill via stdin (empty = no stdin).
//   - timeoutStr: duration string overriding the default 5-minute timeout (empty = use default).
//   - workspaceRoot: propagated as COGOS_WORKSPACE env var (empty = omitted).
//
// Tier-0 and tier-1 skills are rejected with a structured error prefix so
// callers can map them to appropriate HTTP status codes.
//
// Non-zero exit codes are captured in ExecResult.ExitCode and do NOT cause Exec
// to return a non-nil error.
func Exec(ctx context.Context, skill *Record, inputJSON, timeoutStr, workspaceRoot string) (*ExecResult, error) {
	switch skill.Tier {
	case 0:
		return nil, fmt.Errorf("not_executable: skill %q is tier-0 (prose-only). Read its SKILL.md at %s and act per its instructions.", skill.Name, filepath.Join(skill.Path, "SKILL.md"))
	case 1:
		return nil, fmt.Errorf("no_canonical_entry_point: skill %q is tier-1 (scripts present but no canonical exec declared in SKILL.md frontmatter). Promote to tier-2 by adding an exec: field.", skill.Name)
	}

	// Tier 2 or 3.
	if skill.Exec == nil {
		return nil, fmt.Errorf("no_canonical_entry_point: skill %q has no exec declared", skill.Name)
	}

	execPath := filepath.Join(skill.Path, *skill.Exec)
	if _, err := os.Stat(execPath); err != nil {
		return nil, fmt.Errorf("exec_not_found: skill %q exec binary not found at %s", skill.Name, execPath)
	}

	// Resolve timeout: parameter > default 5m.
	timeout := 5 * time.Minute
	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = d
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, execPath)
	if inputJSON != "" {
		cmd.Stdin = bytes.NewBufferString(inputJSON)
	}

	// Propagate identity-relevant env vars.
	cmd.Env = append(os.Environ(),
		"COGOS_SKILL_NAME="+skill.Name,
		"COGOS_SKILL_VERSION="+skill.Version,
		"COGOS_SKILL_PATH="+skill.Path,
	)
	if workspaceRoot != "" {
		cmd.Env = append(cmd.Env, "COGOS_WORKSPACE="+workspaceRoot)
	}

	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	durationMS := time.Since(start).Milliseconds()

	exitCode := 0
	var errMsg string

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			errMsg = fmt.Sprintf("timeout after %s", timeout)
			exitCode = 124 // same as bash timeout
		} else if exitErr, ok := runErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			errMsg = runErr.Error()
		}
	}

	return &ExecResult{
		Name:       skill.Name,
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMS: durationMS,
		Error:      errMsg,
	}, nil
}
