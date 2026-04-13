package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// Modality event type constants for the CogOS ledger.
const (
	EventModalityInput       = "modality.input"
	EventModalityOutput      = "modality.output"
	EventModalityTransform   = "modality.transform"
	EventModalityGate        = "modality.gate"
	EventModalityStateChange = "modality.state_change"
	EventModalityError       = "modality.error"
)

// ModalityInputData is the Data payload for a modality.input event.
type ModalityInputData struct {
	Modality       string  `json:"modality"`
	Channel        string  `json:"channel"`
	Transcript     string  `json:"transcript"`
	GateConfidence float64 `json:"gate_confidence,omitempty"`
	SpeechRatio    float64 `json:"speech_ratio,omitempty"`
	LatencyMs      int     `json:"latency_ms,omitempty"`
	Engine         string  `json:"engine,omitempty"`
}

// ModalityOutputData is the Data payload for a modality.output event.
type ModalityOutputData struct {
	Modality    string  `json:"modality"`
	Channel     string  `json:"channel"`
	Text        string  `json:"text"`
	Engine      string  `json:"engine,omitempty"`
	Voice       string  `json:"voice,omitempty"`
	RTF         float64 `json:"rtf,omitempty"`
	DurationSec float64 `json:"duration_sec,omitempty"`
	LatencyMs   int     `json:"latency_ms,omitempty"`
}

// ModalityTransformData is the Data payload for a modality.transform event.
type ModalityTransformData struct {
	FromModality string `json:"from_modality"`
	ToModality   string `json:"to_modality"`
	Step         string `json:"step"`
	Engine       string `json:"engine,omitempty"`
	LatencyMs    int    `json:"latency_ms,omitempty"`
	InputBytes   int    `json:"input_bytes,omitempty"`
	OutputChars  int    `json:"output_chars,omitempty"`
}

// ModalityGateData is the Data payload for a modality.gate event.
type ModalityGateData struct {
	Modality    string  `json:"modality"`
	Channel     string  `json:"channel"`
	Decision    string  `json:"decision"`
	Confidence  float64 `json:"confidence"`
	SpeechRatio float64 `json:"speech_ratio,omitempty"`
	DurationMs  int     `json:"duration_ms,omitempty"`
	Gate        string  `json:"gate,omitempty"`
}

// ModalityStateChangeData is the Data payload for a modality.state_change event.
type ModalityStateChangeData struct {
	Modality  string `json:"modality"`
	Module    string `json:"module"`
	FromState string `json:"from_state"`
	ToState   string `json:"to_state"`
	PID       int    `json:"pid,omitempty"`
}

// ModalityErrorData is the Data payload for a modality.error event.
type ModalityErrorData struct {
	Modality    string `json:"modality"`
	Module      string `json:"module"`
	Error       string `json:"error"`
	ErrorType   string `json:"error_type,omitempty"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

// newModalityPayload builds an EventPayload from a typed data struct.
func newModalityPayload(eventType, sessionID string, data any) (*EventPayload, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("%s: marshal data: %w", eventType, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: unmarshal data: %w", eventType, err)
	}
	return &EventPayload{
		Type:      eventType,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		SessionID: sessionID,
		Data:      m,
	}, nil
}

// requireFields returns an error naming the first empty field, or nil.
func requireFields(eventType string, pairs ...string) error {
	for i := 0; i < len(pairs)-1; i += 2 {
		if pairs[i+1] == "" {
			return fmt.Errorf("%s: %s is required", eventType, pairs[i])
		}
	}
	return nil
}

// NewModalityInputEvent creates an EventPayload for a modality.input event.
func NewModalityInputEvent(sessionID string, d *ModalityInputData) (*EventPayload, error) {
	if err := requireFields(EventModalityInput,
		"modality", d.Modality, "channel", d.Channel, "transcript", d.Transcript,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityInput, sessionID, d)
}

// NewModalityOutputEvent creates an EventPayload for a modality.output event.
func NewModalityOutputEvent(sessionID string, d *ModalityOutputData) (*EventPayload, error) {
	if err := requireFields(EventModalityOutput,
		"modality", d.Modality, "channel", d.Channel, "text", d.Text,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityOutput, sessionID, d)
}

// NewModalityTransformEvent creates an EventPayload for a modality.transform event.
func NewModalityTransformEvent(sessionID string, d *ModalityTransformData) (*EventPayload, error) {
	if err := requireFields(EventModalityTransform,
		"from_modality", d.FromModality, "to_modality", d.ToModality, "step", d.Step,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityTransform, sessionID, d)
}

// NewModalityGateEvent creates an EventPayload for a modality.gate event.
func NewModalityGateEvent(sessionID string, d *ModalityGateData) (*EventPayload, error) {
	if err := requireFields(EventModalityGate,
		"modality", d.Modality, "channel", d.Channel, "decision", d.Decision,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityGate, sessionID, d)
}

// NewModalityStateChangeEvent creates an EventPayload for a modality.state_change event.
func NewModalityStateChangeEvent(sessionID string, d *ModalityStateChangeData) (*EventPayload, error) {
	if err := requireFields(EventModalityStateChange,
		"modality", d.Modality, "module", d.Module,
		"from_state", d.FromState, "to_state", d.ToState,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityStateChange, sessionID, d)
}

// NewModalityErrorEvent creates an EventPayload for a modality.error event.
func NewModalityErrorEvent(sessionID string, d *ModalityErrorData) (*EventPayload, error) {
	if err := requireFields(EventModalityError,
		"modality", d.Modality, "module", d.Module, "error", d.Error,
	); err != nil {
		return nil, err
	}
	return newModalityPayload(EventModalityError, sessionID, d)
}
