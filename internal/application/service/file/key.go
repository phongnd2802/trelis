// Package file holds the FileService that Source Files are stored with: one
// S3-compatible provider (ADR-0006).
package file

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// DocumentsPrefix is the prefix every Document's objects live under.
const DocumentsPrefix = "documents/"

// SourceKey is the object key of the Source File of one upload of a
// Document. Keys are built from IDs only, never from the uploaded name.
func SourceKey(documentID, uploadID uuid.UUID) string {
	return UploadPrefix(documentID, uploadID) + "source"
}

// DocumentPrefix is the object key prefix of every upload of a Document.
func DocumentPrefix(documentID uuid.UUID) string {
	return fmt.Sprintf("%s%s/", DocumentsPrefix, documentID)
}

// UploadPrefix is the object key prefix of one upload of a Document.
func UploadPrefix(documentID, uploadID uuid.UUID) string {
	return fmt.Sprintf("%s%s/", DocumentPrefix(documentID), uploadID)
}

// ParseKey reads the Document and upload IDs out of an object key under
// DocumentsPrefix. It fails for a key that does not follow the
// documents/{document_id}/{upload_id}/... layout.
func ParseKey(key string) (documentID, uploadID uuid.UUID, err error) {
	rest, ok := strings.CutPrefix(key, DocumentsPrefix)
	if !ok {
		return uuid.Nil, uuid.Nil, fmt.Errorf("key %q is not under %s", key, DocumentsPrefix)
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		return uuid.Nil, uuid.Nil, fmt.Errorf("key %q does not follow the documents/{document_id}/{upload_id} layout", key)
	}
	documentID, err = uuid.Parse(parts[0])
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("key %q document ID: %w", key, err)
	}
	uploadID, err = uuid.Parse(parts[1])
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("key %q upload ID: %w", key, err)
	}
	return documentID, uploadID, nil
}
