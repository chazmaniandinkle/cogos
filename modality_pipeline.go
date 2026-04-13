// modality_pipeline.go — Top-level orchestrator wiring all modality bus
// components together. Provides the single ingress/egress API surface.
//
// Ingress: raw input -> bus.Perceive -> router.RoutePerceive -> CognitiveEvent
// Egress:  intent -> channel lookup -> bus.Act -> router.RouteAct -> outputs
package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
)

// ModalityPipeline is the top-level orchestrator that wires all modality
// bus components together and provides the ingress/egress API.
type ModalityPipeline struct {
	Bus        *ModalityBus
	Channels   *ChannelRegistry
	Router     *EventRouter
	Supervisor *ProcessSupervisor
	HUD        *ModalityHUD
	Salience   *ModalitySalience
	Tools      *ModalityToolSet
	Ledger     *ModalityLedger

	sessionID string
	rootDir   string
}

// PipelineConfig configures the pipeline.
type PipelineConfig struct {
	WorkspaceRoot string
	SessionID     string
	LedgerDir     string // defaults to .cog/ledger/{sessionID}/
}

// NewModalityPipeline creates and wires all modality bus components.
// The text module is always registered. Salience is wired as an event listener.
func NewModalityPipeline(cfg *PipelineConfig) *ModalityPipeline {
	if cfg.LedgerDir == "" {
		cfg.LedgerDir = filepath.Join(cfg.WorkspaceRoot, ".cog", "ledger", cfg.SessionID)
	}

	bus := NewModalityBus()
	channels := NewChannelRegistry()
	ledger := NewModalityLedger(cfg.WorkspaceRoot, cfg.SessionID)
	router := NewEventRouter(bus, ledger, cfg.SessionID)
	supervisor := NewProcessSupervisor(cfg.WorkspaceRoot)
	hud := NewModalityHUD(bus, channels)
	salience := NewModalitySalience()
	tools := NewModalityToolSet(bus)

	// Wire salience as a real-time event listener.
	router.AddListener(salience.OnEvent)

	// Text is the identity modality -- always available.
	if err := bus.Register(NewTextModule()); err != nil {
		log.Printf("modality-pipeline: failed to register text module: %v", err)
	}

	return &ModalityPipeline{
		Bus:        bus,
		Channels:   channels,
		Router:     router,
		Supervisor: supervisor,
		HUD:        hud,
		Salience:   salience,
		Tools:      tools,
		Ledger:     ledger,
		sessionID:  cfg.SessionID,
		rootDir:    cfg.WorkspaceRoot,
	}
}

// Start starts the pipeline: all registered modules begin processing.
func (p *ModalityPipeline) Start(ctx context.Context) error {
	log.Printf("modality-pipeline: starting (session=%s)", p.sessionID)
	if err := p.Bus.Start(ctx); err != nil {
		return fmt.Errorf("modality-pipeline: start: %w", err)
	}
	log.Printf("modality-pipeline: started")
	return nil
}

// Stop stops the pipeline: all registered modules cease processing.
func (p *ModalityPipeline) Stop(ctx context.Context) error {
	log.Printf("modality-pipeline: stopping (session=%s)", p.sessionID)
	if err := p.Bus.Stop(ctx); err != nil {
		return fmt.Errorf("modality-pipeline: stop: %w", err)
	}
	log.Printf("modality-pipeline: stopped")
	return nil
}

// HandleIngress is the single entry point for all inbound modality data.
//
//	raw input -> bus.Perceive(raw, modality, channelID)
//	          -> router.RoutePerceive(event, gateResult)
//	          -> return CognitiveEvent
//
// Returns (nil, nil) when a gate rejects the input (rejection is not an error).
func (p *ModalityPipeline) HandleIngress(channelID string, modality ModalityType, raw []byte) (*CognitiveEvent, error) {
	// Run through the bus: gate -> decode -> CognitiveEvent.
	event, err := p.Bus.Perceive(raw, modality, channelID)
	if err != nil {
		return nil, fmt.Errorf("ingress: %w", err)
	}

	// Gate rejected -- not an error, but nothing to route.
	if event == nil {
		return nil, nil
	}

	// Route through the event system (ledger + listeners).
	if err := p.Router.RoutePerceive(event, nil); err != nil {
		return nil, fmt.Errorf("ingress: route: %w", err)
	}

	return event, nil
}

// HandleEgress routes agent output to all active channels that support
// the target modality.
//
//	intent -> channels.SupportsModality(sessionID, intent.Modality)
//	       -> for each channel: bus.Act(intent with channel set)
//	       -> router.RouteAct(intent, output)
//	       -> return map[channelID]*EncodedOutput
func (p *ModalityPipeline) HandleEgress(sessionID string, intent *CognitiveIntent) (map[string]*EncodedOutput, error) {
	targets := p.Channels.SupportsModality(sessionID, intent.Modality)
	if len(targets) == 0 {
		return nil, fmt.Errorf("egress: no channels support modality %s for session %s", intent.Modality, sessionID)
	}

	outputs := make(map[string]*EncodedOutput, len(targets))
	for _, ch := range targets {
		// Set the channel on a per-target copy so the encoder sees it.
		channelIntent := &CognitiveIntent{
			Modality: intent.Modality,
			Channel:  ch.ID,
			Content:  intent.Content,
			Params:   intent.Params,
		}

		output, err := p.Bus.Act(channelIntent)
		if err != nil {
			return nil, fmt.Errorf("egress: act on channel %s: %w", ch.ID, err)
		}

		if err := p.Router.RouteAct(channelIntent, output); err != nil {
			return nil, fmt.Errorf("egress: route on channel %s: %w", ch.ID, err)
		}

		outputs[ch.ID] = output
	}

	return outputs, nil
}

// Status returns combined pipeline status for diagnostics and the agent HUD.
func (p *ModalityPipeline) Status() map[string]any {
	channelSnap := p.Channels.Snapshot()
	channels := make(map[string]any, len(channelSnap))
	for id, desc := range channelSnap {
		channels[id] = map[string]any{
			"transport": desc.Transport,
			"input":     desc.Input,
			"output":    desc.Output,
		}
	}

	return map[string]any{
		"bus":      p.Bus.HUD(),
		"channels": channels,
		"salience": p.Salience.Snapshot(),
	}
}
