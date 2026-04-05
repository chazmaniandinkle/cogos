package main

// ModalityLedger provides typed methods for emitting modality events
// into the CogOS event ledger.
type ModalityLedger struct {
	workspaceRoot string // path to workspace root (parent of .cog/)
	sessionID     string
}

// NewModalityLedger creates a ModalityLedger bound to a workspace and session.
func NewModalityLedger(workspaceRoot, sessionID string) *ModalityLedger {
	return &ModalityLedger{
		workspaceRoot: workspaceRoot,
		sessionID:     sessionID,
	}
}

// emit is the shared append path: build payload, wrap in envelope, append.
func (ml *ModalityLedger) emit(payload *EventPayload) error {
	env := &EventEnvelope{
		HashedPayload: *payload,
		Metadata:      EventMetadata{Source: "modality-bus"},
	}
	return AppendEvent(ml.workspaceRoot, ml.sessionID, env)
}

// EmitInput records a modality.input event.
func (ml *ModalityLedger) EmitInput(data *ModalityInputData) error {
	p, err := NewModalityInputEvent(ml.sessionID, data)
	if err != nil {
		return err
	}
	return ml.emit(p)
}

// EmitOutput records a modality.output event.
func (ml *ModalityLedger) EmitOutput(data *ModalityOutputData) error {
	p, err := NewModalityOutputEvent(ml.sessionID, data)
	if err != nil {
		return err
	}
	return ml.emit(p)
}

// EmitTransform records a modality.transform event.
func (ml *ModalityLedger) EmitTransform(data *ModalityTransformData) error {
	p, err := NewModalityTransformEvent(ml.sessionID, data)
	if err != nil {
		return err
	}
	return ml.emit(p)
}

// EmitGate records a modality.gate event.
func (ml *ModalityLedger) EmitGate(data *ModalityGateData) error {
	p, err := NewModalityGateEvent(ml.sessionID, data)
	if err != nil {
		return err
	}
	return ml.emit(p)
}

// EmitStateChange records a modality.state_change event.
func (ml *ModalityLedger) EmitStateChange(data *ModalityStateChangeData) error {
	p, err := NewModalityStateChangeEvent(ml.sessionID, data)
	if err != nil {
		return err
	}
	return ml.emit(p)
}

// EmitError records a modality.error event.
func (ml *ModalityLedger) EmitError(data *ModalityErrorData) error {
	p, err := NewModalityErrorEvent(ml.sessionID, data)
	if err != nil {
		return err
	}
	return ml.emit(p)
}
