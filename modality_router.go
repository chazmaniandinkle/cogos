// modality_router.go — EventRouter connects the ModalityBus to the kernel's
// event systems: ledger (persistence), listeners (streaming), salience.
package main

import "sync"

// EventListener receives modality events for real-time processing.
type EventListener func(eventType string, data map[string]any)

// EventRouter connects the ModalityBus to the kernel's event systems.
type EventRouter struct {
	mu        sync.RWMutex
	ledger    *ModalityLedger
	listeners []EventListener
	bus       *ModalityBus
	sessionID string
}

// NewEventRouter creates a router wired to a bus and ledger.
func NewEventRouter(bus *ModalityBus, ledger *ModalityLedger, sessionID string) *EventRouter {
	return &EventRouter{
		bus:       bus,
		ledger:    ledger,
		sessionID: sessionID,
	}
}

// AddListener registers a listener for real-time events.
func (r *EventRouter) AddListener(fn EventListener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners = append(r.listeners, fn)
}

// RoutePerceive routes a perceive result through the ledger and listeners.
// If gateResult is non-nil, a modality.gate event is emitted first.
func (r *EventRouter) RoutePerceive(event *CognitiveEvent, gateResult *GateResult) error {
	// Gate event (optional).
	if gateResult != nil {
		decision := "reject"
		if gateResult.Allowed {
			decision = "accept"
		}
		gd := &ModalityGateData{
			Modality: string(event.Modality), Channel: event.Channel,
			Decision: decision, Confidence: gateResult.Confidence, Gate: gateResult.Reason,
		}
		if err := r.ledger.EmitGate(gd); err != nil {
			return err
		}
		r.notifyListeners(EventModalityGate, map[string]any{
			"modality":   gd.Modality,
			"channel":    gd.Channel,
			"decision":   gd.Decision,
			"confidence": gd.Confidence,
		})
	}

	id := &ModalityInputData{
		Modality: string(event.Modality), Channel: event.Channel,
		Transcript: event.Content, GateConfidence: event.Confidence,
	}
	if err := r.ledger.EmitInput(id); err != nil {
		return err
	}
	r.notifyListeners(EventModalityInput, map[string]any{
		"modality":   id.Modality,
		"channel":    id.Channel,
		"transcript": id.Transcript,
	})
	return nil
}

// RouteAct routes an act result through the ledger and listeners.
func (r *EventRouter) RouteAct(intent *CognitiveIntent, output *EncodedOutput) error {
	od := &ModalityOutputData{
		Modality: string(intent.Modality), Channel: intent.Channel,
		Text: intent.Content, Engine: stringFromMap(intent.Params, "engine"),
		Voice: stringFromMap(intent.Params, "voice"),
	}
	if err := r.ledger.EmitOutput(od); err != nil {
		return err
	}
	r.notifyListeners(EventModalityOutput, map[string]any{
		"modality":  od.Modality,
		"channel":   od.Channel,
		"text":      od.Text,
		"mime_type": output.MimeType,
		"bytes":     len(output.Data),
	})
	return nil
}

// RouteTransform routes a modality transform event.
func (r *EventRouter) RouteTransform(from, to ModalityType, step, engine string, latencyMs int) error {
	td := &ModalityTransformData{
		FromModality: string(from), ToModality: string(to),
		Step: step, Engine: engine, LatencyMs: latencyMs,
	}
	if err := r.ledger.EmitTransform(td); err != nil {
		return err
	}
	r.notifyListeners(EventModalityTransform, map[string]any{
		"from_modality": td.FromModality,
		"to_modality":   td.ToModality,
		"step":          td.Step,
		"engine":        td.Engine,
		"latency_ms":    td.LatencyMs,
	})
	return nil
}

// RouteStateChange routes a module lifecycle state change event.
func (r *EventRouter) RouteStateChange(modality, module, fromState, toState string, pid int) error {
	sd := &ModalityStateChangeData{
		Modality: modality, Module: module,
		FromState: fromState, ToState: toState, PID: pid,
	}
	if err := r.ledger.EmitStateChange(sd); err != nil {
		return err
	}
	r.notifyListeners(EventModalityStateChange, map[string]any{
		"modality":   modality,
		"module":     module,
		"from_state": fromState,
		"to_state":   toState,
		"pid":        pid,
	})
	return nil
}

// RouteError routes a module error event.
func (r *EventRouter) RouteError(modality, module, errMsg, errType string, recoverable bool) error {
	ed := &ModalityErrorData{
		Modality: modality, Module: module, Error: errMsg,
		ErrorType: errType, Recoverable: recoverable,
	}
	if err := r.ledger.EmitError(ed); err != nil {
		return err
	}
	r.notifyListeners(EventModalityError, map[string]any{
		"modality":    modality,
		"module":      module,
		"error":       errMsg,
		"error_type":  errType,
		"recoverable": recoverable,
	})
	return nil
}

// notifyListeners sends an event to all registered listeners.
func (r *EventRouter) notifyListeners(eventType string, data map[string]any) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, fn := range r.listeners {
		fn(eventType, data)
	}
}

// stringFromMap extracts a string value from a map, returning "" if absent.
func stringFromMap(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}
