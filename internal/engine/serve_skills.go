// serve_skills.go — HTTP handlers for skill discovery and execution (issues #96, #97).
//
// Routes:
//   GET  /v1/skills              — list available skills (JSON array)
//   POST /v1/skills/{name}/exec  — invoke a skill by name

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ─── Types (mirrors cmd_skill.go contract) ───────────────────────────────────────

// SkillRecord is the JSON object returned by GET /v1/skills.
type SkillRecord struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Tier        int      `json:"tier"`
	Description string   `json:"description"`
	Exec        *string  `json:"exec"`
	Schema      *string  `json:"schema"`
	Path        string   `json:"path"`
	Requires    []string `json:"requires"`
	Gated       bool     `json:"gated,omitempty"`
}

// skillFM holds parsed SKILL.md front-matter.
type skillFM struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Exec        string   `yaml:"exec"`
	Schema      string   `yaml:"schema"`
	Requires    []string `yaml:"requires"`
	Timeout     string   `yaml:"timeout"`
}

// SkillExecRequest is the POST body for /v1/skills/{name}/exec.
type SkillExecRequest struct {
	Input   string `json:"input"`             // raw JSON to pass via stdin
	Timeout string `json:"timeout,omitempty"` // duration string, e.g. "30s"
}

// SkillExecResponse is the response from /v1/skills/{name}/exec.
type SkillExecResponse struct {
	Name       string `json:"name"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// ─── Route registration ──────────────────────────────────────────────────────────

func (s *Server) registerSkillRoutes(mux *http.ServeMux) {
	s.route(mux, "GET /v1/skills", s.handleSkillList)
	s.route(mux, "POST /v1/skills/{name}/exec", s.handleSkillExec)
}

// ─── Discovery ───────────────────────────────────────────────────────────────────

// skillDirs returns directories to scan, in priority order (workspace overrides user).
func (s *Server) skillDirs() []string {
	var dirs []string

	home, err := os.UserHomeDir()
	if err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "skills"))
	}

	if s.cfg != nil && s.cfg.WorkspaceRoot != "" {
		dirs = append(dirs, filepath.Join(s.cfg.WorkspaceRoot, ".claude", "skills"))
	}

	return dirs
}

// parseSkillFM extracts YAML front-matter from SKILL.md content.
func parseSkillFM(data []byte) (skillFM, string, string) {
	content := string(data)
	var fm skillFM

	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---")
		if end != -1 {
			_ = yaml.Unmarshal([]byte(content[4:4+end]), &fm)
			content = content[4+end+4:]
		}
	}

	title, desc := "", ""
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

// inferSkillTier determines tier from directory structure and front-matter.
func inferSkillTier(skillPath string, fm skillFM) int {
	if fm.Schema != "" && fm.Exec != "" {
		return 3
	}
	if fm.Exec != "" {
		if _, err := os.Stat(filepath.Join(skillPath, fm.Exec)); err == nil {
			return 2
		}
	}
	entries, err := os.ReadDir(skillPath)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".sh") || strings.HasSuffix(n, ".py") {
			return 1
		}
	}
	if binEntries, err := os.ReadDir(filepath.Join(skillPath, "bin")); err == nil && len(binEntries) > 0 && fm.Exec == "" {
		return 1
	}
	return 0
}

// loadSkillRecord converts a directory into a SkillRecord.
func loadSkillRecord(name, skillPath string) (*SkillRecord, error) {
	data, err := os.ReadFile(filepath.Join(skillPath, "SKILL.md"))
	if err != nil {
		return nil, err
	}

	fm, title, desc := parseSkillFM(data)

	skillName := name
	if fm.Name != "" {
		skillName = fm.Name
	}
	skillDesc := fm.Description
	if skillDesc == "" {
		skillDesc = desc
	}
	if skillDesc == "" {
		skillDesc = title
	}
	version := fm.Version
	if version == "" {
		version = "0.1.0"
	}
	tier := inferSkillTier(skillPath, fm)

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
		e := fm.Exec
		rec.Exec = &e
	}
	if fm.Schema != "" {
		sc := fm.Schema
		rec.Schema = &sc
	}
	return rec, nil
}

// discoverSkills returns all skills from the configured directories.
func (s *Server) discoverSkills() ([]SkillRecord, error) {
	seen := make(map[string]SkillRecord)
	var order []string

	for _, dir := range s.skillDirs() {
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
			rec, err := loadSkillRecord(name, filepath.Join(dir, name))
			if err != nil {
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

// findSkill looks up a skill by name.
func (s *Server) findSkill(name string) (*SkillRecord, error) {
	skills, err := s.discoverSkills()
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

// ─── Handlers ────────────────────────────────────────────────────────────────────

func (s *Server) handleSkillList(w http.ResponseWriter, r *http.Request) {
	skills, err := s.discoverSkills()
	if err != nil {
		http.Error(w, `{"error":"discovery_failed","detail":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(skills)
}

func (s *Server) handleSkillExec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	skill, err := s.findSkill(name)
	if err != nil {
		// Same response whether not-found or role-gated (don't leak existence).
		http.Error(w, `{"error":"not_found","detail":"skill not found or not available"}`, http.StatusNotFound)
		return
	}

	var req SkillExecRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid_json"}`, http.StatusBadRequest)
			return
		}
	}

	resp, err := s.execSkill(r.Context(), skill, req.Input, req.Timeout)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not_executable") || strings.Contains(err.Error(), "no_canonical_entry_point") {
			status = http.StatusUnprocessableEntity
		}
		errJSON, _ := json.Marshal(map[string]string{"error": err.Error()})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(errJSON)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// ─── Exec ─────────────────────────────────────────────────────────────────────────

func (s *Server) execSkill(ctx context.Context, skill *SkillRecord, inputJSON, timeoutStr string) (*SkillExecResponse, error) {
	switch skill.Tier {
	case 0:
		return nil, fmt.Errorf("not_executable: skill %q is tier-0 (prose-only). Read SKILL.md at %s", skill.Name, filepath.Join(skill.Path, "SKILL.md"))
	case 1:
		return nil, fmt.Errorf("no_canonical_entry_point: skill %q is tier-1 (no exec: in SKILL.md frontmatter)", skill.Name)
	}

	if skill.Exec == nil {
		return nil, fmt.Errorf("no_canonical_entry_point: skill %q has no exec declared", skill.Name)
	}

	execPath := filepath.Join(skill.Path, *skill.Exec)
	if _, err := os.Stat(execPath); err != nil {
		return nil, fmt.Errorf("exec_not_found: %s", execPath)
	}

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

	env := os.Environ()
	env = append(env,
		"COGOS_SKILL_NAME="+skill.Name,
		"COGOS_SKILL_VERSION="+skill.Version,
		"COGOS_SKILL_PATH="+skill.Path,
	)
	if s.cfg != nil && s.cfg.WorkspaceRoot != "" {
		env = append(env, "COGOS_WORKSPACE="+s.cfg.WorkspaceRoot)
	}
	cmd.Env = env

	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	execErr := cmd.Run()
	durationMS := time.Since(start).Milliseconds()

	exitCode := 0
	var errMsg string

	if execErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			errMsg = fmt.Sprintf("timeout after %s", timeout)
			exitCode = 124
		} else if exitErr, ok := execErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			errMsg = execErr.Error()
		}
	}

	resp := &SkillExecResponse{
		Name:       skill.Name,
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMS: durationMS,
		Error:      errMsg,
	}

	// Emit skill.exec bus event (best-effort)
	s.emitSkillExecEvent(skill, resp)

	return resp, nil
}

// emitSkillExecEvent emits a skill.exec event to the bus. Non-fatal.
func (s *Server) emitSkillExecEvent(skill *SkillRecord, resp *SkillExecResponse) {
	if s.busSessions == nil {
		return
	}
	payload := map[string]interface{}{
		"name":        skill.Name,
		"version":     skill.Version,
		"exit_code":   resp.ExitCode,
		"duration_ms": resp.DurationMS,
	}
	if resp.Error != "" {
		payload["error"] = resp.Error
	}
	_, _ = s.busSessions.AppendEvent("skill", "skill.exec", "kernel:skill:exec", payload)
}
