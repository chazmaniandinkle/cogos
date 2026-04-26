// daemon_test.go — verifies that importing this package registers all 8
// expected Reconcilable providers with pkg/reconcile.
//
// This is the canonical test for verification gate 3: "after the
// engine.Main-equivalent boot path runs, reconcile.ListProviders() returns
// the 8 expected names."
//
// We don't start a server or invoke engine.Main() — we only confirm that
// the package's init() side-effect wired the providers. This is sufficient
// because cmd/cogos/providers_wire.go blank-imports this package, so the
// same init() fires at daemon boot.
package daemon_test

import (
	"sort"
	"testing"

	"github.com/cogos-dev/cogos/pkg/reconcile"

	// Blank import fires daemon.init(), registering all 8 providers.
	_ "github.com/cogos-dev/cogos/internal/providers/daemon"
)

var expectedProviders = []string{
	"agent",
	"component",
	"discord",
	"mcp-tools",
	"openclaw-agents",
	"openclaw-cron",
	"openclaw-gateway",
	"service",
}

func TestDaemonInit_RegistersAll8Providers(t *testing.T) {
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
