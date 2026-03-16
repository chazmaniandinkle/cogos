// Package envspec parses @env-spec decorated .env files and resolves secret
// references from pluggable backends. It is Varlock-compatible: any .env file
// that uses the @env-spec decorator syntax can be parsed and resolved.
//
// The core flow is: Parse → Resolve → use the resulting Env (inject into a
// child process, merge into os.Environ, or read individual values).
//
//	schema, _ := envspec.ParseFile(".envspec")
//	env, _    := envspec.Resolve(ctx, schema, resolver1, resolver2)
//	cmd       := envspec.Exec(env, "cog", "serve")
package envspec

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// SecretRef describes where a single env var's value should come from.
type SecretRef struct {
	Provider string            // e.g. "bitwarden", "literal", "env", "file", "vault-file", "keychain"
	Params   map[string]string // provider-specific params (id, name, path, etc.)
}

// Entry is a single variable declaration in an envspec file.
type Entry struct {
	Key     string    // variable name (e.g. "ANTHROPIC_API_KEY")
	Default string    // default value after the '=' (may be empty)
	Ref     SecretRef // parsed @env-spec decorator
	Line    int       // source line number
}

// Schema is the parsed representation of an envspec file.
type Schema struct {
	Entries []Entry
	Path    string // source file path
}

// Env holds resolved environment variable key-value pairs.
type Env struct {
	Vars map[string]string
}

// Resolver resolves a secret reference to its plaintext value.
type Resolver interface {
	Name() string
	CanResolve(ref SecretRef) bool
	Resolve(ctx context.Context, ref SecretRef) (string, error)
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

// decoratorRe matches lines like: # @env-spec bitwarden(id="abc-123")
var decoratorRe = regexp.MustCompile(
	`^#\s*@env-spec\s+(\w[\w-]*)\s*(?:\(([^)]*)\))?\s*$`,
)

// ParseFile reads an @env-spec decorated file and returns a Schema.
func ParseFile(path string) (*Schema, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("envspec: open %s: %w", path, err)
	}
	defer f.Close()

	schema := &Schema{Path: path}
	scanner := bufio.NewScanner(f)

	var pending *SecretRef
	var pendingLine int
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and plain comments.
		if line == "" {
			continue
		}

		// Check for @env-spec decorator.
		if m := decoratorRe.FindStringSubmatch(line); m != nil {
			provider := m[1]
			params := parseParams(m[2])
			pending = &SecretRef{Provider: provider, Params: params}
			pendingLine = lineNum
			continue
		}

		// Skip non-decorator comments.
		if strings.HasPrefix(line, "#") {
			continue
		}

		// Must be a KEY=VALUE line.
		key, val := splitKV(line)
		if key == "" {
			continue
		}

		entry := Entry{
			Key:     key,
			Default: val,
			Line:    lineNum,
		}

		if pending != nil {
			entry.Ref = *pending
			entry.Line = pendingLine // associate with decorator line
			pending = nil
		} else {
			// No decorator → treat as literal.
			entry.Ref = SecretRef{Provider: "literal"}
		}

		schema.Entries = append(schema.Entries, entry)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("envspec: scan %s: %w", path, err)
	}

	return schema, nil
}

// parseParams parses key="value", key="value" from inside parentheses.
func parseParams(raw string) map[string]string {
	params := make(map[string]string)
	if raw == "" {
		return params
	}
	// Simple state machine: split on commas, then on '='.
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		params[k] = v
	}
	return params
}

// splitKV splits "KEY=VALUE" into key and value.
func splitKV(line string) (string, string) {
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return "", ""
	}
	return strings.TrimSpace(k), strings.TrimSpace(v)
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// Resolve resolves all entries in a schema using the provided resolvers.
// Resolvers are tried in order; the first that can resolve a ref wins.
// If no resolver can handle a ref and the entry has a non-empty default,
// the default is used. Otherwise an error is returned.
func Resolve(ctx context.Context, schema *Schema, resolvers ...Resolver) (*Env, error) {
	env := &Env{Vars: make(map[string]string, len(schema.Entries))}

	for _, entry := range schema.Entries {
		val, err := resolveEntry(ctx, entry, resolvers)
		if err != nil {
			return nil, fmt.Errorf("envspec: %s (line %d): %w", entry.Key, entry.Line, err)
		}
		env.Vars[entry.Key] = val
	}

	return env, nil
}

func resolveEntry(ctx context.Context, entry Entry, resolvers []Resolver) (string, error) {
	// Literal provider: use the default value directly.
	if entry.Ref.Provider == "literal" {
		return entry.Default, nil
	}

	for _, r := range resolvers {
		if !r.CanResolve(entry.Ref) {
			continue
		}
		val, err := r.Resolve(ctx, entry.Ref)
		if err != nil {
			continue // try next resolver
		}
		return val, nil
	}

	// Fall back to default if available.
	if entry.Default != "" {
		return entry.Default, nil
	}

	return "", fmt.Errorf("no resolver could handle provider %q", entry.Ref.Provider)
}

// ---------------------------------------------------------------------------
// Environment helpers
// ---------------------------------------------------------------------------

// Pairs returns the env as a []string suitable for exec.Cmd.Env.
func (e *Env) Pairs() []string {
	pairs := make([]string, 0, len(e.Vars))
	for k, v := range e.Vars {
		pairs = append(pairs, k+"="+v)
	}
	return pairs
}

// Merge adds all vars from other into e, overwriting on conflict.
func (e *Env) Merge(other *Env) {
	for k, v := range other.Vars {
		e.Vars[k] = v
	}
}

// MergeOS returns os.Environ() with the envspec vars overlaid.
func (e *Env) MergeOS() []string {
	existing := make(map[string]string)
	for _, pair := range os.Environ() {
		k, v, _ := strings.Cut(pair, "=")
		existing[k] = v
	}
	for k, v := range e.Vars {
		existing[k] = v
	}
	result := make([]string, 0, len(existing))
	for k, v := range existing {
		result = append(result, k+"="+v)
	}
	return result
}

// Get returns a single variable's value.
func (e *Env) Get(key string) string {
	return e.Vars[key]
}

// Exec creates an exec.Cmd with the resolved env merged into os.Environ().
func Exec(env *Env, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Env = env.MergeOS()
	return cmd
}

// Chain creates a ChainResolver that tries resolvers in order.
func Chain(resolvers ...Resolver) *ChainResolver {
	return &ChainResolver{resolvers: resolvers}
}

// ChainResolver tries multiple resolvers in order.
type ChainResolver struct {
	resolvers []Resolver
}

func (c *ChainResolver) Name() string { return "chain" }

func (c *ChainResolver) CanResolve(ref SecretRef) bool {
	for _, r := range c.resolvers {
		if r.CanResolve(ref) {
			return true
		}
	}
	return false
}

func (c *ChainResolver) Resolve(ctx context.Context, ref SecretRef) (string, error) {
	var lastErr error
	for _, r := range c.resolvers {
		if !r.CanResolve(ref) {
			continue
		}
		val, err := r.Resolve(ctx, ref)
		if err != nil {
			lastErr = err
			continue
		}
		return val, nil
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("no resolver in chain could handle provider %q", ref.Provider)
}
