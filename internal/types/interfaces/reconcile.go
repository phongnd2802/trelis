package interfaces

import "context"

// ReconcileService runs one pass over the state that lives outside a Postgres
// transaction: Documents left pending (ADR-0005) and objects left without a
// Document or from a replaced upload (ADR-0006).
type ReconcileService interface {
	// Reconcile submits the ingest start for every pending Document and deletes
	// object prefixes no Document or current upload uses. Every step is
	// idempotent, so any number of processes may run a pass at once.
	Reconcile(ctx context.Context) error
}
