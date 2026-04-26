// eval_wiring.go — DI wiring for the internal/eval package.
//
// Populates the EvalProvider dependency-injection seams (NewEvalProvider, NowISO)
// and registers the eval provider with pkg/reconcile.
//
// Pattern mirrors component_wiring.go: blank import on the eval package
// ensures its init() runs; explicit references wire the seams.
//
// NOTE: This file is in the `cog` CLI binary's main package. The kernel daemon
// does NOT import this directly. Agent 1's parallel work on bridging the kernel
// daemon to the workspace-root reconcile registry will pick up this registration.
//
// Phase C: EvalProvider is registered with reconcile and its seams are wired.
// The concrete HTTP-backed BusReader and BusEmitter implementations are used
// when available; tests can inject stubs via the seam variables.

package main

import (
	"time"

	"github.com/cogos-dev/cogos/internal/eval"
	"github.com/cogos-dev/cogos/pkg/reconcile"
)

func init() {
	// Wire the NowISO dependency so EvalProvider uses the same timestamp
	// function as the rest of the cog CLI binary.
	eval.NowISO = func() string {
		return time.Now().UTC().Format(time.RFC3339)
	}

	// Wire the EvalProvider constructor. The concrete BusReader is an HTTP reader
	// hitting the local kernel; the BusEmitter is an HTTP emitter. Both degrade
	// gracefully when the kernel is not reachable.
	eval.NewEvalProvider = func(dispatcher eval.AgentDispatcher, emitter eval.BusEmitter, reader eval.CogdocReader) *eval.EvalProvider {
		return eval.New(dispatcher, emitter, reader)
	}

	// Register the eval provider with the reconcile registry.
	// The provider's Reconcilable methods are now fully implemented (Phase C).
	reconcile.RegisterProvider("eval", eval.New(nil, nil, nil))
}
