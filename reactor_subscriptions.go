// reactor_subscriptions.go - CRD eventSubscription → ReactorRule generator.
//
// Reads agent CRDs and converts their scheduling.eventSubscriptions into
// ReactorRules, bridging the declarative agent config with the deterministic
// reactor. When a subscription fires, the triggering event is forwarded to
// the subscribing agent's bus endpoint for processing.

package main

import (
	"fmt"
	"log"
)

// GenerateSubscriptionRules reads all agent CRDs and creates ReactorRules
// for each eventSubscription declared in the CRD's scheduling section.
// When a rule fires, it forwards the triggering event to the agent's bus
// endpoint via the bus session manager.
func GenerateSubscriptionRules(root string, bus *busSessionManager) ([]ReactorRule, error) {
	crds, err := ListAgentCRDs(root)
	if err != nil {
		return nil, fmt.Errorf("generate subscription rules: %w", err)
	}

	var rules []ReactorRule

	for _, crd := range crds {
		agentName := crd.Metadata.Name
		agentEndpoint := crd.Spec.Bus.Endpoint
		subs := crd.Spec.Scheduling.EventSubscriptions

		for _, sub := range subs {
			// Determine the event type pattern for matching.
			// Prefer Filter (which supports globs from B1) over Type.
			eventType := sub.Filter
			if eventType == "" {
				eventType = sub.Type
			}
			if eventType == "" {
				continue // no usable event pattern — skip
			}

			// Build the rule name: "{agent}.sub.{subscription-type}"
			ruleName := fmt.Sprintf("%s.sub.%s", agentName, sub.Type)

			// Capture for closure
			capturedAgent := agentName
			capturedEventType := eventType
			capturedEndpoint := agentEndpoint
			capturedSubType := sub.Type

			rule := ReactorRule{
				Name:      ruleName,
				EventType: eventType,
				BusFilter: sub.Channel, // empty string = match any bus
				Action: func(block *CogBlock) {
					log.Printf("[reactor-sub] %s triggered by %s on bus=%s",
						capturedAgent, capturedEventType, block.BusID)

					// Forward the event to the agent's bus endpoint for processing
					if bus != nil && capturedEndpoint != "" {
						payload := map[string]interface{}{
							"triggeredBy":      block.Type,
							"originalFrom":     block.From,
							"originalBusID":    block.BusID,
							"subscriptionType": capturedSubType,
							"agentTarget":      capturedAgent,
						}
						if _, err := bus.appendBusEvent(capturedEndpoint, "agent.subscription.triggered", "kernel:reactor", payload); err != nil {
							log.Printf("[reactor-sub] failed to forward to %s: %v", capturedEndpoint, err)
						}
					}
				},
			}

			rules = append(rules, rule)
		}
	}

	return rules, nil
}
