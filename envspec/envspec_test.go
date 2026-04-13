package envspec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseFile(t *testing.T) {
	content := `# Application secrets
# @env-spec bitwarden(id="abc-123-def")
ANTHROPIC_API_KEY=

# @env-spec bitwarden(id="def-456-ghi")
DISCORD_TOKEN=

# @env-spec literal
COG_NODE_ID=desktop-win11

# @env-spec env(name="HOME")
USER_HOME=

# No decorator — treated as literal
SOME_FLAG=true

# @env-spec file(path="/run/secrets/db_pass")
DB_PASSWORD=
`

	dir := t.TempDir()
	path := filepath.Join(dir, ".envspec")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	schema, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if len(schema.Entries) != 6 {
		t.Fatalf("expected 6 entries, got %d", len(schema.Entries))
	}

	tests := []struct {
		key      string
		provider string
		paramKey string
		paramVal string
		defVal   string
	}{
		{"ANTHROPIC_API_KEY", "bitwarden", "id", "abc-123-def", ""},
		{"DISCORD_TOKEN", "bitwarden", "id", "def-456-ghi", ""},
		{"COG_NODE_ID", "literal", "", "", "desktop-win11"},
		{"USER_HOME", "env", "name", "HOME", ""},
		{"SOME_FLAG", "literal", "", "", "true"},
		{"DB_PASSWORD", "file", "path", "/run/secrets/db_pass", ""},
	}

	for i, tt := range tests {
		entry := schema.Entries[i]
		if entry.Key != tt.key {
			t.Errorf("entry[%d]: key = %q, want %q", i, entry.Key, tt.key)
		}
		if entry.Ref.Provider != tt.provider {
			t.Errorf("entry[%d] %s: provider = %q, want %q", i, tt.key, entry.Ref.Provider, tt.provider)
		}
		if tt.paramKey != "" {
			if got := entry.Ref.Params[tt.paramKey]; got != tt.paramVal {
				t.Errorf("entry[%d] %s: param %q = %q, want %q", i, tt.key, tt.paramKey, got, tt.paramVal)
			}
		}
		if entry.Default != tt.defVal {
			t.Errorf("entry[%d] %s: default = %q, want %q", i, tt.key, entry.Default, tt.defVal)
		}
	}
}

func TestResolve_Literal(t *testing.T) {
	content := `# @env-spec literal
MY_VAR=hello-world
`
	dir := t.TempDir()
	path := filepath.Join(dir, ".envspec")
	os.WriteFile(path, []byte(content), 0644)

	schema, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	env, err := Resolve(context.Background(), schema)
	if err != nil {
		t.Fatal(err)
	}

	if got := env.Get("MY_VAR"); got != "hello-world" {
		t.Errorf("MY_VAR = %q, want %q", got, "hello-world")
	}
}

func TestResolve_EnvFallback(t *testing.T) {
	os.Setenv("TEST_ENVSPEC_VAR", "from-env")
	defer os.Unsetenv("TEST_ENVSPEC_VAR")

	content := `# @env-spec env(name="TEST_ENVSPEC_VAR")
MY_VAR=
`
	dir := t.TempDir()
	path := filepath.Join(dir, ".envspec")
	os.WriteFile(path, []byte(content), 0644)

	schema, _ := ParseFile(path)
	env, err := Resolve(context.Background(), schema, NewEnvResolver())
	if err != nil {
		t.Fatal(err)
	}

	if got := env.Get("MY_VAR"); got != "from-env" {
		t.Errorf("MY_VAR = %q, want %q", got, "from-env")
	}
}

func TestResolve_FileResolver(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret.txt")
	os.WriteFile(secretPath, []byte("super-secret\n"), 0600)

	content := `# @env-spec file(path="` + secretPath + `")
SECRET=
`
	envPath := filepath.Join(dir, ".envspec")
	os.WriteFile(envPath, []byte(content), 0644)

	schema, _ := ParseFile(envPath)
	env, err := Resolve(context.Background(), schema, NewFileResolver())
	if err != nil {
		t.Fatal(err)
	}

	if got := env.Get("SECRET"); got != "super-secret" {
		t.Errorf("SECRET = %q, want %q", got, "super-secret")
	}
}

func TestResolve_VaultFile(t *testing.T) {
	vaultJSON := `{
		"items": [
			{
				"id": "abc-123",
				"name": "anthropic-key",
				"login": {"password": "sk-ant-xxx"},
				"notes": ""
			},
			{
				"id": "def-456",
				"name": "discord-token",
				"login": {"password": ""},
				"notes": "MTIzNDU2"
			}
		]
	}`

	resolver, err := NewVaultFileResolver([]byte(vaultJSON))
	if err != nil {
		t.Fatal(err)
	}

	content := `# @env-spec bitwarden(id="abc-123")
API_KEY=
# @env-spec bitwarden(id="def-456")
TOKEN=
`
	dir := t.TempDir()
	path := filepath.Join(dir, ".envspec")
	os.WriteFile(path, []byte(content), 0644)

	schema, _ := ParseFile(path)
	env, err := Resolve(context.Background(), schema, resolver)
	if err != nil {
		t.Fatal(err)
	}

	if got := env.Get("API_KEY"); got != "sk-ant-xxx" {
		t.Errorf("API_KEY = %q, want %q", got, "sk-ant-xxx")
	}
	if got := env.Get("TOKEN"); got != "MTIzNDU2" {
		t.Errorf("TOKEN = %q, want %q", got, "MTIzNDU2")
	}
}

func TestChainResolver(t *testing.T) {
	vaultJSON := `{"items": [{"id": "abc-123", "name": "test", "login": {"password": "from-vault"}, "notes": ""}]}`
	vaultResolver, _ := NewVaultFileResolver([]byte(vaultJSON))

	chain := Chain(vaultResolver, NewEnvResolver())

	content := `# @env-spec bitwarden(id="abc-123")
MY_SECRET=
`
	dir := t.TempDir()
	path := filepath.Join(dir, ".envspec")
	os.WriteFile(path, []byte(content), 0644)

	schema, _ := ParseFile(path)
	env, err := Resolve(context.Background(), schema, chain)
	if err != nil {
		t.Fatal(err)
	}

	if got := env.Get("MY_SECRET"); got != "from-vault" {
		t.Errorf("MY_SECRET = %q, want %q", got, "from-vault")
	}
}

func TestEnv_Pairs(t *testing.T) {
	env := &Env{Vars: map[string]string{
		"A": "1",
		"B": "2",
	}}

	pairs := env.Pairs()
	if len(pairs) != 2 {
		t.Fatalf("expected 2 pairs, got %d", len(pairs))
	}

	found := make(map[string]bool)
	for _, p := range pairs {
		found[p] = true
	}
	if !found["A=1"] || !found["B=2"] {
		t.Errorf("unexpected pairs: %v", pairs)
	}
}

func TestEnv_Merge(t *testing.T) {
	a := &Env{Vars: map[string]string{"X": "1", "Y": "2"}}
	b := &Env{Vars: map[string]string{"Y": "3", "Z": "4"}}
	a.Merge(b)

	if a.Get("X") != "1" {
		t.Error("X should be 1")
	}
	if a.Get("Y") != "3" {
		t.Error("Y should be overwritten to 3")
	}
	if a.Get("Z") != "4" {
		t.Error("Z should be 4")
	}
}

func TestResolve_DefaultFallback(t *testing.T) {
	// A bitwarden ref with no available resolver should fall back to default.
	content := `# @env-spec bitwarden(id="nonexistent")
FALLBACK_VAR=my-default
`
	dir := t.TempDir()
	path := filepath.Join(dir, ".envspec")
	os.WriteFile(path, []byte(content), 0644)

	schema, _ := ParseFile(path)
	env, err := Resolve(context.Background(), schema) // no resolvers
	if err != nil {
		t.Fatal(err)
	}

	if got := env.Get("FALLBACK_VAR"); got != "my-default" {
		t.Errorf("FALLBACK_VAR = %q, want %q", got, "my-default")
	}
}
