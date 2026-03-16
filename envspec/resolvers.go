package envspec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// LiteralResolver — uses the default value as-is
// ---------------------------------------------------------------------------

type LiteralResolver struct{}

func NewLiteralResolver() *LiteralResolver { return &LiteralResolver{} }

func (r *LiteralResolver) Name() string                { return "literal" }
func (r *LiteralResolver) CanResolve(ref SecretRef) bool { return ref.Provider == "literal" }
func (r *LiteralResolver) Resolve(_ context.Context, _ SecretRef) (string, error) {
	return "", fmt.Errorf("literal resolver should not be called directly; default value is used in resolution")
}

// ---------------------------------------------------------------------------
// EnvResolver — resolves from existing environment variables
// ---------------------------------------------------------------------------

type EnvResolver struct{}

func NewEnvResolver() *EnvResolver { return &EnvResolver{} }

func (r *EnvResolver) Name() string                { return "env" }
func (r *EnvResolver) CanResolve(ref SecretRef) bool { return ref.Provider == "env" }

func (r *EnvResolver) Resolve(_ context.Context, ref SecretRef) (string, error) {
	name := ref.Params["name"]
	if name == "" {
		return "", fmt.Errorf("env resolver requires 'name' param")
	}
	val, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("env var %q not set", name)
	}
	return val, nil
}

// ---------------------------------------------------------------------------
// FileResolver — reads secret from a file (e.g. Docker secrets at /run/secrets/)
// ---------------------------------------------------------------------------

type FileResolver struct{}

func NewFileResolver() *FileResolver { return &FileResolver{} }

func (r *FileResolver) Name() string                { return "file" }
func (r *FileResolver) CanResolve(ref SecretRef) bool { return ref.Provider == "file" }

func (r *FileResolver) Resolve(_ context.Context, ref SecretRef) (string, error) {
	path := ref.Params["path"]
	if path == "" {
		return "", fmt.Errorf("file resolver requires 'path' param")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("file resolver: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ---------------------------------------------------------------------------
// BitwardenResolver — resolves from Bitwarden/Vaultwarden Secrets Manager API
// ---------------------------------------------------------------------------

// BitwardenResolver resolves secrets from a Bitwarden Secrets Manager API.
// This targets Vaultwarden's /api/secrets/{id} endpoint.
type BitwardenResolver struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewBitwardenResolver creates a resolver that talks to a Bitwarden/Vaultwarden
// Secrets Manager API. The baseURL should include the scheme and host
// (e.g. "http://localhost:8222"). The token is a machine account access token.
func NewBitwardenResolver(baseURL, token string) *BitwardenResolver {
	return &BitwardenResolver{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (r *BitwardenResolver) Name() string { return "bitwarden" }

func (r *BitwardenResolver) CanResolve(ref SecretRef) bool {
	return ref.Provider == "bitwarden"
}

func (r *BitwardenResolver) Resolve(ctx context.Context, ref SecretRef) (string, error) {
	id := ref.Params["id"]
	if id == "" {
		return "", fmt.Errorf("bitwarden resolver requires 'id' param")
	}

	url := fmt.Sprintf("%s/api/secrets/%s", r.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("bitwarden: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("bitwarden: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bitwarden: HTTP %d for secret %s", resp.StatusCode, id)
	}

	var result struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("bitwarden: decode response: %w", err)
	}

	return result.Value, nil
}

// ---------------------------------------------------------------------------
// BitwardenCLIResolver — resolves via the `bw` CLI tool
// ---------------------------------------------------------------------------

type BitwardenCLIResolver struct {
	bwPath string // path to bw binary; empty = find in $PATH
}

func NewBitwardenCLIResolver() *BitwardenCLIResolver {
	return &BitwardenCLIResolver{}
}

func (r *BitwardenCLIResolver) Name() string { return "bitwarden-cli" }

func (r *BitwardenCLIResolver) CanResolve(ref SecretRef) bool {
	return ref.Provider == "bitwarden-cli"
}

func (r *BitwardenCLIResolver) Resolve(ctx context.Context, ref SecretRef) (string, error) {
	name := ref.Params["name"]
	if name == "" {
		return "", fmt.Errorf("bitwarden-cli resolver requires 'name' param")
	}

	bw := r.bwPath
	if bw == "" {
		bw = "bw"
	}

	cmd := exec.CommandContext(ctx, bw, "get", "password", name)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("bitwarden-cli: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// ---------------------------------------------------------------------------
// VaultFileResolver — resolves from a local encrypted Bitwarden vault export
// ---------------------------------------------------------------------------

// VaultFileResolver resolves secrets from a locally decrypted Bitwarden vault
// export. The vault file is a JSON export that has been decrypted with the
// master password. Entries are looked up by UUID.
//
// For the full multi-node flow: the encrypted vault.enc is synced via BlockSync.
// At node startup, the kernel decrypts it once using the master key from the OS
// keychain, and passes the resulting JSON to this resolver.
type VaultFileResolver struct {
	entries map[string]vaultEntry // keyed by UUID
}

type vaultEntry struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Login struct {
		Password string `json:"password"`
	} `json:"login"`
	Notes string `json:"notes"`
}

type vaultExport struct {
	Items []vaultEntry `json:"items"`
}

// NewVaultFileResolver creates a resolver from a decrypted Bitwarden JSON export.
// The jsonData should be the decrypted vault contents (not the encrypted file).
func NewVaultFileResolver(jsonData []byte) (*VaultFileResolver, error) {
	var export vaultExport
	if err := json.Unmarshal(jsonData, &export); err != nil {
		return nil, fmt.Errorf("vault-file: parse export: %w", err)
	}

	entries := make(map[string]vaultEntry, len(export.Items))
	for _, item := range export.Items {
		entries[item.ID] = item
	}

	return &VaultFileResolver{entries: entries}, nil
}

func (r *VaultFileResolver) Name() string { return "vault-file" }

func (r *VaultFileResolver) CanResolve(ref SecretRef) bool {
	// Can resolve bitwarden refs by ID if we have the vault file loaded.
	return ref.Provider == "bitwarden" || ref.Provider == "vault-file"
}

func (r *VaultFileResolver) Resolve(_ context.Context, ref SecretRef) (string, error) {
	id := ref.Params["id"]
	if id == "" {
		id = ref.Params["name"]
	}
	if id == "" {
		return "", fmt.Errorf("vault-file: requires 'id' or 'name' param")
	}

	entry, ok := r.entries[id]
	if !ok {
		// Try matching by name.
		for _, e := range r.entries {
			if e.Name == id {
				entry = e
				ok = true
				break
			}
		}
	}

	if !ok {
		return "", fmt.Errorf("vault-file: entry %q not found", id)
	}

	if entry.Login.Password != "" {
		return entry.Login.Password, nil
	}
	return entry.Notes, nil
}

// ---------------------------------------------------------------------------
// KeychainResolver — resolves from OS keychain (write-through cache)
// ---------------------------------------------------------------------------

// KeychainResolver resolves secrets from the OS keychain. It also serves as a
// write-through cache: when other resolvers succeed, their results can be
// stored here for offline fallback.
//
// On macOS: uses `security find-generic-password` / `security add-generic-password`
// On Linux: uses `secret-tool` (libsecret / gnome-keyring)
// On Windows: uses `cmdkey` (Windows Credential Manager)
type KeychainResolver struct {
	service string // keychain service name (e.g. "cogos-secrets")
}

func NewKeychainResolver(service string) *KeychainResolver {
	return &KeychainResolver{service: service}
}

func (r *KeychainResolver) Name() string { return "keychain" }

func (r *KeychainResolver) CanResolve(ref SecretRef) bool {
	// Keychain can attempt to resolve any provider type as a cache fallback.
	return true
}

func (r *KeychainResolver) Resolve(ctx context.Context, ref SecretRef) (string, error) {
	account := keychainAccount(ref)
	return r.get(ctx, account)
}

// Store saves a resolved value to the OS keychain for later fallback.
func (r *KeychainResolver) Store(ctx context.Context, ref SecretRef, value string) error {
	account := keychainAccount(ref)
	return r.set(ctx, account, value)
}

func keychainAccount(ref SecretRef) string {
	// Use provider:id as the account key.
	id := ref.Params["id"]
	if id == "" {
		id = ref.Params["name"]
	}
	return ref.Provider + ":" + id
}

// get retrieves a password from the OS keychain. macOS implementation.
func (r *KeychainResolver) get(ctx context.Context, account string) (string, error) {
	// macOS: security find-generic-password
	cmd := exec.CommandContext(ctx, "security", "find-generic-password",
		"-s", r.service,
		"-a", account,
		"-w", // output password only
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("keychain: not found: %s/%s", r.service, account)
	}
	return strings.TrimSpace(string(out)), nil
}

// set stores a password in the OS keychain. macOS implementation.
func (r *KeychainResolver) set(ctx context.Context, account, value string) error {
	// Try to update first, add if it doesn't exist.
	// macOS: security add-generic-password -U (update if exists)
	cmd := exec.CommandContext(ctx, "security", "add-generic-password",
		"-s", r.service,
		"-a", account,
		"-w", value,
		"-U", // update if exists
	)
	return cmd.Run()
}
