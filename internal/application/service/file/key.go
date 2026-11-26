// Package file holds the FileService that Source Files are stored with: one
// S3-compatible provider (ADR-0006).
package file

import (
	"fmt"

	"github.com/google/uuid"
)

// SourceKey is the object key of the Source File of one upload of a
// Document. Keys are built from IDs only, never from the uploaded name.
func SourceKey(documentID, uploadID uuid.UUID) string {
	return fmt.Sprintf("documents/%s/%s/source", documentID, uploadID)
}
