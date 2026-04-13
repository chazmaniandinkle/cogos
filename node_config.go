// node_config.go — Multi-node deployment configuration.
//
// Defines the node configuration schema (ADR-063) for workspace-spanning
// multi-node deployments with selective sync, secret resolution, and
// OCI-native container management.

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// ─── Node Configuration Schema ─────────────────────────────────────────────────

// NodeConfig is the top-level node configuration stored at .cog/config/node/node.json.
type NodeConfig struct {
	NodeID      string          `json:"node_id"`
	WorkspaceID string          `json:"workspace_id"`
	Hostname    string          `json:"hostname"`
	Platform    string          `json:"platform"`  // e.g. "darwin/arm64", "linux/amd64"
	Tier        string          `json:"tier"`       // "micro", "standard", "primary"
	Created     string          `json:"created"`
	Services    ServiceConfig   `json:"services"`
	Sidecars    SidecarConfig   `json:"sidecars"`
	Secrets     SecretsConfig   `json:"secrets"`
	Sync        SyncConfig      `json:"sync"`
	Network     NetworkConfig   `json:"network"`
}

// ServiceConfig defines required services that run on every node.
type ServiceConfig struct {
	Kernel  ContainerSpec `json:"kernel"`
	Gateway ContainerSpec `json:"gateway"`
}

// ContainerSpec defines a container image + its configuration.
type ContainerSpec struct {
	Image     string            `json:"image"`
	Ports     map[string]string `json:"ports,omitempty"`     // host:container
	Volumes   []string          `json:"volumes,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	DependsOn []string          `json:"depends_on,omitempty"`
	Always    bool              `json:"always"`
}

// SidecarConfig defines optional sidecar containers.
type SidecarConfig struct {
	Vaultwarden *ContainerSpec `json:"vaultwarden,omitempty"`
	TTS         *ContainerSpec `json:"tts,omitempty"`
}

// SecretsConfig defines how secrets are resolved on this node.
type SecretsConfig struct {
	Provider   string `json:"provider"`             // "envspec"
	SchemaFile string `json:"schema_file"`           // path to .envspec file
	VaultURL   string `json:"vault_url,omitempty"`   // Vaultwarden API URL
	VaultFile  string `json:"vault_file,omitempty"`  // path to vault.enc for offline resolution
}

// SyncConfig defines what this node syncs and how.
type SyncConfig struct {
	Profile   string   `json:"profile"`             // "full", "build", "mobile", "edge", custom
	Include   []string `json:"include"`
	Exclude   []string `json:"exclude"`
	Discovery string   `json:"discovery"`           // "mdns", "static"
	Peers     []string `json:"peers,omitempty"`     // static peer addresses
}

// NetworkConfig defines network settings for this node.
type NetworkConfig struct {
	KernelPort  int    `json:"kernel_port"`
	GatewayPort int    `json:"gateway_port"`
	MDNSEnabled bool   `json:"mdns_enabled"`
	BindAddr    string `json:"bind_addr"`           // "0.0.0.0" for LAN, "127.0.0.1" for local
}

// ─── Sync Profiles ──────────────────────────────────────────────────────────────

// SyncProfilePresets returns predefined sync profiles.
func SyncProfilePresets() map[string]SyncConfig {
	return map[string]SyncConfig{
		"full": {
			Profile:   "full",
			Include:   []string{"**"},
			Exclude:   []string{"**/node_modules/**", "**/.git/**", "**/*.tmp"},
			Discovery: "mdns",
		},
		"build": {
			Profile: "build",
			Include: []string{
				".cog/mem/**",
				".cog/config/**",
				".cog/ontology/**",
				"apps/**",
				"build-tools/**",
				"research/**",
				"CLAUDE.md", "SOUL.md", "USER.md", "IDENTITY.md",
			},
			Exclude: []string{
				"**/node_modules/**",
				"**/.git/**",
				"**/*.tmp",
			},
			Discovery: "mdns",
		},
		"mobile": {
			Profile: "mobile",
			Include: []string{
				".cog/mem/**",
				".cog/config/**",
				".cog/ontology/**",
				"apps/*/bin/**",
				"research/**",
				"CLAUDE.md", "SOUL.md", "USER.md", "IDENTITY.md",
			},
			Exclude: []string{
				"**/node_modules/**",
				"**/.git/**",
				"build-tools/**",
			},
			Discovery: "mdns",
		},
		"edge": {
			Profile: "edge",
			Include: []string{
				".cog/mem/**",
				".cog/config/**",
				"CLAUDE.md", "SOUL.md",
			},
			Exclude:   []string{"**/.git/**"},
			Discovery: "mdns",
		},
	}
}

// ─── Config Path Helpers ────────────────────────────────────────────────────────

// nodeConfigDir returns .cog/config/node/ within the workspace.
func nodeConfigDir(root string) string {
	return filepath.Join(root, ".cog", "config", "node")
}

// nodeConfigPath returns the full path to node.json.
func nodeConfigPath(root string) string {
	return filepath.Join(nodeConfigDir(root), "node.json")
}

// nodeKeyDir returns .cog/config/node/keys/ for Ed25519 keypair storage.
func nodeKeyDir(root string) string {
	return filepath.Join(nodeConfigDir(root), "keys")
}

// ─── Config Operations ──────────────────────────────────────────────────────────

// LoadNodeConfig reads the node configuration from the workspace.
func LoadNodeConfig(root string) (*NodeConfig, error) {
	path := nodeConfigPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load node config: %w", err)
	}
	var cfg NodeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse node config: %w", err)
	}
	return &cfg, nil
}

// SaveNodeConfig writes the node configuration to the workspace.
func SaveNodeConfig(root string, cfg *NodeConfig) error {
	dir := nodeConfigDir(root)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create node config dir: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal node config: %w", err)
	}

	path := nodeConfigPath(root)
	return os.WriteFile(path, data, 0644)
}

// ─── Node Identity ──────────────────────────────────────────────────────────────

// GenerateNodeID creates a new Ed25519 keypair and derives a node ID from
// the public key hash. Keys are stored in .cog/config/node/keys/.
func GenerateNodeID(root string) (string, error) {
	keyDir := nodeKeyDir(root)
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return "", fmt.Errorf("create key dir: %w", err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate keypair: %w", err)
	}

	// Write private key (restricted permissions)
	privPath := filepath.Join(keyDir, "node.key")
	if err := os.WriteFile(privPath, priv, 0600); err != nil {
		return "", fmt.Errorf("write private key: %w", err)
	}

	// Write public key
	pubPath := filepath.Join(keyDir, "node.pub")
	if err := os.WriteFile(pubPath, pub, 0644); err != nil {
		return "", fmt.Errorf("write public key: %w", err)
	}

	// Derive node ID from SHA-256 of public key (first 8 bytes as hex)
	hash := sha256.Sum256(pub)
	nodeID := hex.EncodeToString(hash[:8])

	return nodeID, nil
}

// ─── Default Config Generation ──────────────────────────────────────────────────

// NewDefaultNodeConfig generates a default node configuration for the given tier.
func NewDefaultNodeConfig(nodeID, workspaceID, tier string) *NodeConfig {
	hostname, _ := os.Hostname()
	platform := runtime.GOOS + "/" + runtime.GOARCH

	cfg := &NodeConfig{
		NodeID:      nodeID,
		WorkspaceID: workspaceID,
		Hostname:    hostname,
		Platform:    platform,
		Tier:        tier,
		Created:     time.Now().UTC().Format(time.RFC3339),
		Services: ServiceConfig{
			Kernel: ContainerSpec{
				Image:  "cogos/kernel:latest",
				Ports:  map[string]string{"5100": "5100"},
				Always: true,
			},
			Gateway: ContainerSpec{
				Image:     "cogos/gateway:latest",
				Ports:     map[string]string{"18789": "18789", "18790": "18790"},
				DependsOn: []string{"kernel"},
				Always:    true,
			},
		},
		Secrets: SecretsConfig{
			Provider:   "envspec",
			SchemaFile: ".envspec",
		},
		Network: NetworkConfig{
			KernelPort:  5100,
			GatewayPort: 18789,
			MDNSEnabled: true,
			BindAddr:    "0.0.0.0",
		},
	}

	// Apply sync profile based on tier
	switch tier {
	case "primary":
		cfg.Sync = SyncProfilePresets()["full"]
		cfg.Sidecars.Vaultwarden = &ContainerSpec{
			Image:  "vaultwarden/server:latest",
			Ports:  map[string]string{"8222": "80"},
			Always: true,
		}
		cfg.Secrets.VaultURL = "http://localhost:8222"
	case "standard":
		cfg.Sync = SyncProfilePresets()["build"]
	case "micro":
		cfg.Sync = SyncProfilePresets()["edge"]
		cfg.Services.Gateway = ContainerSpec{} // No gateway on micro
	default:
		cfg.Sync = SyncProfilePresets()["full"]
	}

	return cfg
}
