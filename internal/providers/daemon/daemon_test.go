// daemon_test.go — verifies that importing this package registers all 9
// expected Reconcilable providers with pkg/reconcile.
//
// This is the canonical test for verification gate 3: "after the
// engine.Main-equivalent boot path runs, reconcile.ListProviders() returns
// the 9 expected names."
//
// We don't start a server or invoke engine.Main() — we only confirm that
// the package's init() side-effect wired the providers. This is sufficient
// because cmd/cogos/providers_wire.go imports this package, so the same
// init() fires at daemon boot.
package daemon_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/cogos-dev/cogos/internal/providers/daemon"
	"github.com/cogos-dev/cogos/pkg/reconcile"

	// Blank import fires daemon.init(), registering all 9 providers; the named
	// import above is for the new SetWorkspaceRoot regression test.
	_ "github.com/cogos-dev/cogos/internal/providers/daemon"
)

var expectedProviders = []string{
	"agent",
	"component",
	"discord",
	"eval",
	"mcp-tools",
	"openclaw-agents",
	"openclaw-cron",
	"openclaw-gateway",
	"service",
}

func TestDaemonInit_RegistersAll9Providers(t *testing.T) {
	got := reconcile.ListProviders()
	sort.Strings(got)

	want := make([]string, len(expectedProviders))
	copy(want, expectedProviders)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("provider count: got %d want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}

	missing := []string{}
	for _, name := range want {
		if !reconcile.HasProvider(name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("missing providers after daemon init: %v\nregistered: %v", missing, got)
	}
}

func TestDaemonProviders_TypeMethodsMatchNames(t *testing.T) {
	for _, name := range expectedProviders {
		p, err := reconcile.GetProvider(name)
		if err != nil {
			t.Errorf("GetProvider(%q): %v", name, err)
			continue
		}
		if got := p.Type(); got != name {
			t.Errorf("provider %q: Type()=%q, want %q", name, got, name)
		}
	}
}

func TestDaemonProviders_HealthDoesNotPanic(t *testing.T) {
	for _, name := range expectedProviders {
		p, err := reconcile.GetProvider(name)
		if err != nil {
			t.Errorf("GetProvider(%q): %v", name, err)
			continue
		}
		// Health() must not panic. We don't assert on the return value
		// because it depends on filesystem state (workspace presence,
		// ~/.openclaw/openclaw.json, etc.) which varies by environment.
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("provider %q Health() panicked: %v", name, r)
				}
			}()
			_ = p.Health()
		}()
	}
}

// TestSetWorkspaceRoot_HealthReflectsWorkspaceState verifies that after
// SetWorkspaceRoot is called with a real workspace path, providers that depend
// only on filesystem presence (agent, service) return a non-"workspace-not-found"
// status. This is the core regression test for the daemon workspace seam fix.
func TestSetWorkspaceRoot_HealthReflectsWorkspaceState(t *testing.T) {
	// Build a minimal fake workspace: <tmp>/.cog/bin/agents/registry.yaml
	// so the agent provider can return Healthy.
	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".cog", "bin", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	registryPath := filepath.Join(agentsDir, "registry.yaml")
	if err := os.WriteFile(registryPath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write registry.yaml: %v", err)
	}

	// Before SetWorkspaceRoot: agent Health() should say workspace-not-found
	// (or at least not be Healthy).
	daemon.SetWorkspaceRoot("")
	p, err := reconcile.GetProvider("agent")
	if err != nil {
		t.Fatalf("GetProvider(agent): %v", err)
	}
	before := p.Health()
	if before.Health == reconcile.HealthHealthy {
		t.Error("agent provider unexpectedly Healthy before SetWorkspaceRoot — test environment has a real workspace wired")
	}

	// After SetWorkspaceRoot with our fake workspace: agent should be Healthy.
	daemon.SetWorkspaceRoot(tmp)
	after := p.Health()
	if after.Health != reconcile.HealthHealthy {
		t.Errorf("agent provider Health()=%s after SetWorkspaceRoot(%s); want Healthy; message=%q",
			after.Health, tmp, after.Message)
	}

	// service provider should also be Healthy (no services dir = no services = fine).
	svc, err := reconcile.GetProvider("service")
	if err != nil {
		t.Fatalf("GetProvider(service): %v", err)
	}
	svcStatus := svc.Health()
	if svcStatus.Health != reconcile.HealthHealthy {
		t.Errorf("service provider Health()=%s; want Healthy; message=%q",
			svcStatus.Health, svcStatus.Message)
	}

	// Reset for other tests.
	daemon.SetWorkspaceRoot("")
}
