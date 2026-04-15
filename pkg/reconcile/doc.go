// Package reconcile provides the plan/apply reconciliation framework for CogOS.
//
// Reconciliation is the core control-plane pattern: providers declare desired
// state, the reconciler diffs against actual state, produces a plan of changes,
// and applies them idempotently. This is the same Terraform-style loop used
// throughout the CogOS kernel, extracted as an importable library.
package reconcile
