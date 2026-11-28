package config

import (
	"fmt"
	"time"
)

// Reconciler configures the periodic pass that restarts ingest workflows for
// Documents left pending and deletes objects that outlived their row
// (ADR-0005, ADR-0006). Every server process runs it.
type Reconciler struct {
	// Interval is how often one pass runs.
	Interval time.Duration `mapstructure:"interval"`
}

func init() {
	defaults["reconciler.interval"] = time.Minute
}

// validate reports why the reconciler cannot run as written.
func (r Reconciler) validate() []error {
	if r.Interval <= 0 {
		return []error{fmt.Errorf("reconciler.interval must be greater than 0, got %v", r.Interval)}
	}
	return nil
}
