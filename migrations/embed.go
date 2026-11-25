// Package migrations holds the goose SQL migrations for the Trelis database.
// They live at the repo root so they are easy to find and review; the files
// are embedded into the binary and applied by internal/db on start.
//
// Add a migration as NNNNN_description.sql with `-- +goose Up` and
// `-- +goose Down` sections.
package migrations

import "embed"

// FS holds every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS
