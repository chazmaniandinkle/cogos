package main

import (
	"context"
	"strings"
	"testing"
)

func TestModalityHUD_EmptyBus(t *testing.T) {
	hud := NewModalityHUD(NewModalityBus(), NewChannelRegistry())
	out := hud.Format()
	for _, want := range []string{"Modules:** none", "Channels:** none", "Recent:** none"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestModalityHUD_WithModules(t *testing.T) {
	bus := NewModalityBus()
	m := NewTextModule()
	if err := bus.Register(m); err != nil {
		t.Fatal(err)
	}
	_ = m.Start(context.Background())
	out := NewModalityHUD(bus, NewChannelRegistry()).Format()
	if !strings.Contains(out, "text (healthy)") {
		t.Errorf("missing 'text (healthy)' in:\n%s", out)
	}
}

func TestModalityHUD_WithChannels(t *testing.T) {
	reg := NewChannelRegistry()
	_ = reg.Register(&ChannelDescriptor{
		ID: "claude-code", Input: []ModalityType{ModalityText}, Output: []ModalityType{ModalityText},
	})
	out := NewModalityHUD(NewModalityBus(), reg).Format()
	for _, want := range []string{"claude-code", "text-in", "text-out"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestModalityHUD_Format(t *testing.T) {
	bus := NewModalityBus()
	m := NewTextModule()
	_ = bus.Register(m)
	_ = m.Start(context.Background())
	out := NewModalityHUD(bus, NewChannelRegistry()).Format()
	if !strings.HasPrefix(out, "## Modality Bus Status") {
		t.Error("missing ## header")
	}
	for _, label := range []string{"**Modules:**", "**Channels:**", "**Recent:**"} {
		if !strings.Contains(out, label) {
			t.Errorf("missing label %s", label)
		}
	}
	if w := len(strings.Fields(out)); w > 200 {
		t.Errorf("output exceeds 200 words: %d", w)
	}
}

func TestModalityHUD_TokenEstimate(t *testing.T) {
	bus := NewModalityBus()
	_ = bus.Register(NewTextModule())
	est := NewModalityHUD(bus, NewChannelRegistry()).TokenEstimate()
	if est <= 0 || est >= 500 {
		t.Errorf("token estimate out of range: %d (want 0 < est < 500)", est)
	}
}
