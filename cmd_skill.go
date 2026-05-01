// cmd_skill.go — CLI commands for the CogOS skill system (issues #96, #97).
//
// Commands:
//   cog skill list [--json]    — Enumerate available skills from ~/.claude/skills/ and <workspace>/.claude/skills/
//   cog skill exec <name> [--input <json>] [--timeout <duration>]  — Invoke a tier-2/3 skill
//   cog skill help             — Show usage

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ─── Types ──────────────────────────────────────────────────────────────────────

// SkillRecord is the JSON schema returned by `cog skill list --json` and
// GET /v1/skills. Every field present in the issue contract is represented.
type SkillRecord struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Tier        int      `json:"tier"`
	Description string   `json:"description"`
	Exec        *string  `json:"exec"`         // null for tier-0/1
	Schema      *string  `json:"schema"`       // null unless tier-3
	Path        string   `json:"path"`
	Requires    []string `json:"requires"`
	Gated       bool     `json:"gated,omitempty"` // true when capability-filtered
}

// skillFrontmatter holds the YAML front-matter fields we parse from SKILL.md.
type skillFrontmatter struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Exec        string   `yaml:"exec"`
	Schema      string   `yaml:"schema"`
	Requires    []string `yaml:"requires"`
	Timeout     string   `yaml:"timeout"` // e.g. "5m"
}

// SkillExecResult is the structured response from `cog skill exec`.
type SkillExecResult struct {
	Name       string `json:"name"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// ─── Discovery ──────────────────────────────────────────────────────────────────

// skillDirs returns the ordered list of directories to scan for skills.
// User-level (~/.claude/skills/) is searched first; workspace-level
// (<workspace>/.claude/skills/) overlays it (workspace skills win on same name).
func skillDirs() []string {
	var dirs []string

	// User-level
	home, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "skills"))
	}

	// Workspace-level (project-scoped; may not exist)
	if root, _, err := ResolveWorkspace(); err == nil {
		wsSkills := filepath.Join(root, ".claude", "skills")
		dirs = append(dirs, wsSkills)
	}

	return dirs
}

// parseSkillMD extracts YAML front-matter from a SKILL.md file. The
// front-matter is optional (many existing skills are bare Markdown); callers
// receive a zero-value struct plus the title/description extracted from the
// Markdown heading if no YAML block is present.
func parseSkillMD(data []byte) (skillFrontmatter, string, string) {
	content := string(data)
	var fm skillFrontmatter

	// Extract YAML front-matter between leading --- delimiters.
	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---")
		if end != -1 {
			yamlBlock := content[4 : 4+end]
			_ = yaml.Unmarshal([]byte(yamlBlock), &fm)
			content = content[4+end+4:] // skip past closing ---\n
		}
	}

	// Pull title and description from Markdown heading / first paragraph.
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

// inferTier determines the tier of a skill from its directory layout.
//
//   - Tier 0: prose-only SKILL.md (default)
//   - Tier 1: helper scripts present but no declared exec
//   - Tier 2: exec field set in frontmatter and bin/<exec> exists
//   - Tier 3: tier-2 + schema field set
func inferTier(skillPath string, fm skillFrontmatter) int {
	if fm.Schema != "" {
		// Only tier-3 if there is also a canonical exec
		if fm.Exec != "" {
			return 3
		}
	}

	if fm.Exec != "" {
		// Require the binary to actually exist
		binPath := filepath.Join(skillPath, fm.Exec)
		if _, err := os.Stat(binPath); err == nil {
			return 2
		}
	}

	// Check for any scripts that make this tier-1
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
	// Check bin/ directory for scripts
	binDir := filepath.Join(skillPath, "bin")
	if binEntries, err := os.ReadDir(binDir); err == nil && len(binEntries) > 0 {
		// bin/ exists but exec not declared — tier 1
		if fm.Exec == "" {
			return 1
		}
	}

	return 0
}

// loadSkillRecord converts a skill directory into a SkillRecord.
func loadSkillRecord(name, skillPath string) (*SkillRecord, error) {
	skillFile := filepath.Join(skillPath, "SKILL.md")
	data, err := os.ReadFile(skillFile)
	if err != nil {
		return nil, fmt.Errorf("read SKILL.md: %w", err)
	}

	fm, title, desc := parseSkillMD(data)

	// Resolve name: frontmatter wins; fall back to directory name.
	skillName := name
	if fm.Name != "" {
		skillName = fm.Name
	}

	// Resolve description: frontmatter wins; fall back to Markdown extraction.
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

	tier := inferTier(skillPath, fm)

	rec := &SkillRecord{
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
		exec := fm.Exec
		rec.Exec = &exec
	}
	if fm.Schema != "" {
		schema := fm.Schema
		rec.Schema = &schema
	}

	return rec, nil
}

// DiscoverSkills walks skill directories and returns a deduplicated list of
// SkillRecords. Workspace-level skills override user-level skills with the
// same name (last directory wins).
func DiscoverSkills() ([]SkillRecord, error) {
	seen := make(map[string]SkillRecord)
	var order []string // preserve insertion order for stable output

	for _, dir := range skillDirs() {
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
			skillPath := filepath.Join(dir, name)

			rec, err := loadSkillRecord(name, skillPath)
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

	result := make([]SkillRecord, 0, len(order))
	for _, name := range order {
		result = append(result, seen[name])
	}
	return result, nil
}

// FindSkill looks up a single skill by name; returns an error when not found.
func FindSkill(name string) (*SkillRecord, error) {
	skills, err := DiscoverSkills()
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

// ─── Dispatcher ─────────────────────────────────────────────────────────────────

func cmdSkill(args []string) error {
	if len(args) == 0 {
		return cmdSkillHelp()
	}

	switch args[0] {
	case "list":
		return cmdSkillList(args[1:])
	case "exec":
		return cmdSkillExec(args[1:])
	case "help", "-h", "--help":
		return cmdSkillHelp()
	default:
		return fmt.Errorf("unknown skill command: %s (try: cog skill help)", args[0])
	}
}

// ─── list ───────────────────────────────────────────────────────────────────────

func cmdSkillList(args []string) error {
	jsonMode := false
	for _, arg := range args {
		if arg == "--json" || arg == "-j" {
			jsonMode = true
		}
	}

	skills, err := DiscoverSkills()
	if err != nil {
		return fmt.Errorf("discover skills: %w", err)
	}

	if jsonMode {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(skills)
	}

	// Human-readable table
	if len(skills) == 0 {
		fmt.Println("No skills found.")
		fmt.Println()
		fmt.Println("Add a skill directory with SKILL.md to:")
		home, _ := os.UserHomeDir()
		fmt.Printf("  %s/.claude/skills/<name>/SKILL.md\n", home)
		return nil
	}

	fmt.Printf("%-30s %-8s %s\n", "NAME", "TIER", "DESCRIPTION")
	fmt.Println(strings.Repeat("-", 80))
	for _, s := range skills {
		desc := s.Description
		if len(desc) > 40 {
			desc = desc[:37] + "..."
		}
		fmt.Printf("%-30s tier-%-3d %s\n", s.Name, s.Tier, desc)
	}
	fmt.Printf("\n%d skill(s) found. Use --json for full records.\n", len(skills))
	return nil
}

// ─── exec ───────────────────────────────────────────────────────────────────────

func cmdSkillExec(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cog skill exec <name> [--input <json>] [--timeout <duration>]")
	}

	skillName := args[0]
	inputJSON := ""
	timeoutStr := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--input", "-i":
			if i+1 >= len(args) {
				return fmt.Errorf("--input requires a value")
			}
			i++
			inputJSON = args[i]
		case "--timeout", "-t":
			if i+1 >= len(args) {
				return fmt.Errorf("--timeout requires a value")
			}
			i++
			timeoutStr = args[i]
		default:
			if strings.HasPrefix(args[i], "--input=") {
				inputJSON = strings.TrimPrefix(args[i], "--input=")
			} else if strings.HasPrefix(args[i], "--timeout=") {
				timeoutStr = strings.TrimPrefix(args[i], "--timeout=")
			}
		}
	}

	skill, err := FindSkill(skillName)
	if err != nil {
		// Return the same error shape whether not-found or role-gated.
		return fmt.Errorf("skill not found or not available: %s", skillName)
	}

	result, err := execSkill(skill, inputJSON, timeoutStr)
	if err != nil {
		return err
	}

	// Print result as JSON
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(result); encErr != nil {
		return encErr
	}

	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
	return nil
}

// execSkill is the core exec logic, shared by CLI and HTTP handler.
func execSkill(skill *SkillRecord, inputJSON, timeoutStr string) (*SkillExecResult, error) {
	switch skill.Tier {
	case 0:
		return nil, fmt.Errorf("not_executable: skill %q is tier-0 (prose-only). Read its SKILL.md at %s and act per its instructions.", skill.Name, filepath.Join(skill.Path, "SKILL.md"))
	case 1:
		return nil, fmt.Errorf("no_canonical_entry_point: skill %q is tier-1 (scripts present but no canonical exec declared in SKILL.md frontmatter). Promote to tier-2 by adding an exec: field.", skill.Name)
	}

	// Tier 2 or 3
	if skill.Exec == nil {
		return nil, fmt.Errorf("no_canonical_entry_point: skill %q has no exec declared", skill.Name)
	}

	execPath := filepath.Join(skill.Path, *skill.Exec)
	if _, err := os.Stat(execPath); err != nil {
		return nil, fmt.Errorf("exec_not_found: skill %q exec binary not found at %s", skill.Name, execPath)
	}

	// Determine timeout: flag > frontmatter > default 5m
	timeout := 5 * time.Minute
	if timeoutStr != "" {
		if d, err := time.ParseDuration(timeoutStr); err == nil {
			timeout = d
		}
	}

	// Build context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, execPath)

	// Pass input via stdin
	if inputJSON != "" {
		cmd.Stdin = bytes.NewBufferString(inputJSON)
	}

	// Propagate identity-relevant env vars
	cmd.Env = append(os.Environ(),
		"COGOS_SKILL_NAME="+skill.Name,
		"COGOS_SKILL_VERSION="+skill.Version,
		"COGOS_SKILL_PATH="+skill.Path,
	)
	if root, _, err := ResolveWorkspace(); err == nil {
		cmd.Env = append(cmd.Env, "COGOS_WORKSPACE="+root)
	}

	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	durationMS := time.Since(start).Milliseconds()

	exitCode := 0
	var errMsg string

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			errMsg = fmt.Sprintf("timeout after %s", timeout)
			exitCode = 124 // same as bash timeout
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			errMsg = err.Error()
		}
	}

	result := &SkillExecResult{
		Name:       skill.Name,
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMS: durationMS,
		Error:      errMsg,
	}

	// Emit skill.exec bus event (best-effort; don't fail the exec if the bus is unavailable)
	emitSkillExecEvent(skill, result)

	return result, nil
}

// emitSkillExecEvent sends a skill.exec event to the kernel bus. Non-fatal on error.
func emitSkillExecEvent(skill *SkillRecord, result *SkillExecResult) {
	root, _, err := ResolveWorkspace()
	if err != nil {
		return
	}

	mgr := newBusSessionManager(root)
	payload := map[string]interface{}{
		"name":        skill.Name,
		"version":     skill.Version,
		"exit_code":   result.ExitCode,
		"duration_ms": result.DurationMS,
	}
	if result.Error != "" {
		payload["error"] = result.Error
	}

	_, _ = mgr.appendBusEvent("skill", "skill.exec", "cog:skill:exec", payload)
}

// ─── help ───────────────────────────────────────────────────────────────────────

func cmdSkillHelp() error {
	fmt.Println("Usage: cog skill <command>")
	fmt.Println()
	fmt.Println("Skill Discovery and Execution:")
	fmt.Println("  list [--json]                   List available skills (--json for machine-readable output)")
	fmt.Println("  exec <name> [flags]             Execute a tier-2/3 skill by name")
	fmt.Println()
	fmt.Println("Exec Flags:")
	fmt.Println("  --input, -i <json>              Pass JSON payload to skill via stdin")
	fmt.Println("  --timeout, -t <duration>        Override default 5m exec timeout (e.g. 30s, 2m)")
	fmt.Println()
	fmt.Println("Tier Conventions:")
	fmt.Println("  Tier 0  Prose-only SKILL.md; read and act per its instructions")
	fmt.Println("  Tier 1  Helper scripts present; no canonical entry point declared")
	fmt.Println("  Tier 2  exec: declared in front-matter; bin/<exec> exists")
	fmt.Println("  Tier 3  Tier-2 + schema: declared (typed input/output contract)")
	fmt.Println()
	fmt.Println("Skill Directories:")
	home, _ := os.UserHomeDir()
	fmt.Printf("  User-level:      %s/.claude/skills/\n", home)
	fmt.Println("  Workspace-level: <workspace>/.claude/skills/")
	fmt.Println()
	fmt.Println("Example SKILL.md front-matter (tier-2):")
	fmt.Println("  ---")
	fmt.Println("  name: my-skill")
	fmt.Println("  version: 0.1.0")
	fmt.Println("  description: What this skill does")
	fmt.Println("  exec: bin/run")
	fmt.Println("  requires: []")
	fmt.Println("  ---")
	return nil
}
