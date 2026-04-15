// Package modality implements the bus and pipeline abstractions for multi-modal
// communication in CogOS.
//
// A modality bus routes typed messages (text, voice, structured data) between
// producers and consumers. Pipelines compose transforms over a bus channel,
// enabling codec chains, format negotiation, and channel multiplexing.
package modality
