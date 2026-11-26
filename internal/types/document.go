// Copyright (C) 2026 Tencent. All rights reserved.
// DuplicateDocumentError and NewDuplicateDocumentError are adapted from
// WeKnora (https://github.com/Tencent/WeKnora), MIT License, by way of Omnisc
// (internal/types/errors.go, DuplicateKnowledgeError). Modified for Trelis:
// renamed for Documents per GLOSSARY.md. See NOTICE.

package types

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Parse statuses of a Document. Names follow Omnisc's Knowledge.ParseStatus.
const (
	ParseStatusPending    = "pending"
	ParseStatusProcessing = "processing"
	ParseStatusCompleted  = "completed"
	ParseStatusFailed     = "failed"
)

// Document is one uploaded file, the unit access is granted on. It mirrors
// the file fields of Omnisc's Knowledge, without knowledge bases or tenants:
// the Access List decides who may read it.
type Document struct {
	// Unique identifier of the Document
	ID uuid.UUID `json:"id"`
	// OwnerID is the Principal who uploaded it; always on the Access List.
	OwnerID uuid.UUID `json:"-"`
	// Title of the Document; the file name at upload
	Title string `json:"title"`
	// File name of the Document
	FileName string `json:"file_name"`
	// File type of the Document: its lower-case extension
	FileType string `json:"file_type"`
	// File size of the Document in bytes
	FileSize int64 `json:"file_size"`
	// File hash of the Document: hex SHA-256 of its content
	FileHash string `json:"file_hash"`
	// UploadID names the upload the current Source File came from; each
	// upload writes under its own key prefix (ADR-0006).
	UploadID uuid.UUID `json:"-"`
	// ObjectKey is the Source File's key in object storage. Not shown to
	// clients: they read it through GET /documents/{id}/file, never the bucket.
	ObjectKey string `json:"-"`
	// Parse status of the Document
	ParseStatus string `json:"parse_status"`
	// ParseError is why the last parse failed; empty unless ParseStatus is
	// failed. It names no file name or Document text.
	ParseError string `json:"parse_error,omitempty"`
	// Creation time of the Document
	CreatedAt time.Time `json:"created_at"`
	// Last updated time of the Document
	UpdatedAt time.Time `json:"updated_at"`
	// AccessList is filled in only for the owner, so other readers do not
	// learn who else can read the Document.
	AccessList *AccessList `json:"access_list,omitempty"`
}

// AccessList names the Principals and Groups allowed to read a Document.
type AccessList struct {
	Principals []string `json:"principals"`
	Groups     []string `json:"groups"`
}

// AccessGrants is an Access List resolved to IDs, in the order its names
// were given.
type AccessGrants struct {
	PrincipalIDs []uuid.UUID
	GroupIDs     []uuid.UUID
}

// DuplicateDocumentError means the owner already uploaded a file with the
// same content; Document is that earlier upload.
type DuplicateDocumentError struct {
	Message  string
	Document *Document
}

func (e *DuplicateDocumentError) Error() string {
	return e.Message
}

// NewDuplicateDocumentError creates a duplicate Document error
func NewDuplicateDocumentError(doc *Document) *DuplicateDocumentError {
	return &DuplicateDocumentError{
		Message:  fmt.Sprintf("Document already exists: %s", doc.FileName),
		Document: doc,
	}
}
