package engine

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// NodeManifest is the single source of truth for services on this node.
type NodeManifest struct {
	APIVersion string                `yaml:"apiVersion" json:"apiVersion"`
	Kind       string                `yaml:"kind" json:"kind"`
	Services   map[string]ServiceDef `yaml:"services" json:"services"`
}

// ServiceKind classifies a service by ownership and observability.
//
//   - "managed"  (default): kernel orchestrates the lifecycle via a supervisor.
//   - "observed": kernel probes health but does not control start/stop/restart.
//   - "external": kernel knows the service exists for dependency-graph purposes
//     only; it does not probe and does not control.
type ServiceKind string

const (
	ServiceKindManaged  ServiceKind = "managed"
	ServiceKindObserved ServiceKind = "observed"
	ServiceKindExternal ServiceKind = "external"
)

// EffectiveKind returns the resolved kind, defaulting to "managed" when unset.
func (k ServiceKind) EffectiveKind() ServiceKind {
	if k == "" {
		return ServiceKindManaged
	}
	return k
}

// ServiceDef describes a single managed service.
type ServiceDef struct {
	Kind      ServiceKind     `yaml:"kind,omitempty" json:"kind,omitempty"`
	Port      int             `yaml:"port" json:"port"`
	Binary    string          `yaml:"binary,omitempty" json:"binary,omitempty"`
	Workdir   string          `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	Venv      string          `yaml:"venv,omitempty" json:"venv,omitempty"`
	Command   string          `yaml:"command" json:"command"`
	Health    string          `yaml:"health" json:"health"`
	Restart   string          `yaml:"restart" json:"restart"`
	Launchd   string          `yaml:"launchd,omitempty" json:"launchd,omitempty"`
	DependsOn []string        `yaml:"depends_on" json:"depends_on"`
	Consumers []ConsumerEntry `yaml:"consumers,omitempty" json:"consumers,omitempty"`
}

// ConsumerEntry declares a file that references this service's port.
type ConsumerEntry struct {
	Path     string `yaml:"path" json:"path"`
	Type     string `yaml:"type" json:"type"` // json, sed, plist
	JSONPath string `yaml:"jsonpath,omitempty" json:"jsonpath,omitempty"`
	Template string `yaml:"template,omitempty" json:"template,omitempty"`
	Match    string `yaml:"match,omitempty" json:"match,omitempty"`
	Replace  string `yaml:"replace,omitempty" json:"replace,omitempty"`
	Key      string `yaml:"key,omitempty" json:"key,omitempty"`
}

// LoadManifest reads and parses a manifest.yaml file.
func LoadManifest(path string) (*NodeManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m NodeManifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// DefaultManifestPath returns the expected manifest location for a workspace.
func DefaultManifestPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".cog", "config", "node", "manifest.yaml")
}

// resolveManifestWorkspace resolves the workspace root from a flag or cwd.
func resolveManifestWorkspace(ws string) (string, error) {
	if ws != "" {
		return ws, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return findWorkspaceRoot(wd)
}

// runManifestCmd implements `cogos manifest` — emits the parsed manifest as JSON.
func runManifestCmd(args []string, defaultWorkspace string) {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	workspace := fs.String("workspace", defaultWorkspace, "Workspace root path")
	service := fs.String("service", "", "Emit only this service (optional)")
	_ = fs.Parse(args)

	ws, err := resolveManifestWorkspace(*workspace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	m, err := LoadManifest(DefaultManifestPath(ws))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	var out any
	if *service != "" {
		svc, ok := m.Services[*service]
		if !ok {
			fmt.Fprintf(os.Stderr, "error: service %q not found in manifest\n", *service)
			os.Exit(1)
		}
		out = svc
	} else {
		out = m
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
