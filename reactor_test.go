package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ─── Test 1: matchEventType unit tests ──────────────────────────────────────

func TestMatchEventType(t *testing.T) {
	cases := []struct {
		pattern, eventType string
		want               bool
	}{
		{"system.startup", "system.startup", true},
		{"system.startup", "system.shutdown", false},
		{"agent.*", "agent.capabilities", true},
		{"agent.*", "agent.event.processed", true}, // path.Match "*" matches non-"/" chars; "." is not a separator
		{"*.error", "chat.error", true},
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "other", false},
	}
	for _, tc := range cases {
		got := matchEventType(tc.pattern, tc.eventType)
		if got != tc.want {
			t.Errorf("matchEventType(%q, %q) = %v, want %v",
				tc.pattern, tc.eventType, got, tc.want)
		}
	}
}

// ─── Test 2: Reactor fires on glob-matched event ────────────────────────────

func TestReactorGlobRuleFires(t *testing.T) {
	mgr := newBusSessionManager(t.TempDir())
	reactor := NewReactor(mgr)

	fired := make(chan string, 1)
	reactor.AddRule(ReactorRule{
		Name:      "test-glob",
		EventType: "agent.*",
		Action:    func(block *CogBlock) { fired <- block.Type },
	})
	reactor.Start()
	defer reactor.Stop()

	// Simulate an event dispatch through the bus manager's handler list.
	dispatch := func(busID string, block *CogBlock) {
		mgr.mu.Lock()
		handlers := make([]busEventHandler, len(mgr.eventHandlers))
		copy(handlers, mgr.eventHandlers)
		mgr.mu.Unlock()
		for _, h := range handlers {
			h.handler(busID, block)
		}
	}

	// Should fire: matches "agent.*"
	dispatch("bus1", &CogBlock{Type: "agent.capabilities"})
	select {
	case got := <-fired:
		if got != "agent.capabilities" {
			t.Errorf("rule fired with type %q, want %q", got, "agent.capabilities")
		}
	case <-time.After(time.Second):
		t.Fatal("rule did not fire for agent.capabilities within timeout")
	}

	// Should NOT fire: "tool.invoke" does not match "agent.*"
	dispatch("bus1", &CogBlock{Type: "tool.invoke"})
	select {
	case got := <-fired:
		t.Errorf("rule unexpectedly fired for tool.invoke, got %q", got)
	case <-time.After(100 * time.Millisecond):
		// expected — no fire
	}
}

// ─── Test 3: GenerateSubscriptionRules from CRD YAML ────────────────────────

func TestGenerateSubscriptionRules(t *testing.T) {
	// Build a temp workspace with a test agent CRD.
	root := t.TempDir()
	defDir := filepath.Join(root, ".cog", "bin", "agents", "definitions")
	if err := os.MkdirAll(defDir, 0755); err != nil {
		t.Fatal(err)
	}

	yaml := `apiVersion: cog.os/v1alpha1
kind: Agent
metadata:
  name: testagent
spec:
  type: headless
  scheduling:
    eventSubscriptions:
      - type: system.startup
        filter: "system.*"
        channel: "#ops"
      - type: agent.error
`
	if err := os.WriteFile(filepath.Join(defDir, "testagent.agent.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	rules, err := GenerateSubscriptionRules(root, nil)
	if err != nil {
		t.Fatalf("GenerateSubscriptionRules: %v", err)
	}

	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}

	// First rule: filter takes precedence over type, so EventType = "system.*"
	if rules[0].Name != "testagent.sub.system.startup" {
		t.Errorf("rules[0].Name = %q, want %q", rules[0].Name, "testagent.sub.system.startup")
	}
	if rules[0].EventType != "system.*" {
		t.Errorf("rules[0].EventType = %q, want %q", rules[0].EventType, "system.*")
	}
	if rules[0].BusFilter != "#ops" {
		t.Errorf("rules[0].BusFilter = %q, want %q", rules[0].BusFilter, "#ops")
	}

	// Second rule: no filter, falls back to type
	if rules[1].Name != "testagent.sub.agent.error" {
		t.Errorf("rules[1].Name = %q, want %q", rules[1].Name, "testagent.sub.agent.error")
	}
	if rules[1].EventType != "agent.error" {
		t.Errorf("rules[1].EventType = %q, want %q", rules[1].EventType, "agent.error")
	}
	if rules[1].BusFilter != "" {
		t.Errorf("rules[1].BusFilter = %q, want empty", rules[1].BusFilter)
	}
}
