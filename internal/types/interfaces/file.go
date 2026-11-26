// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc. Modified for Trelis: no tenants, URLs, temp
// files or copies; objects are keyed by Document and upload ID (ADR-0006).
// See NOTICE.

// Package interfaces declares the service, repository and storage contracts
// the handler, service and repository layers meet at.
package interfaces

import (
	"context"
	"errors"
	"io"
	"mime/multipart"

	"github.com/google/uuid"
)

// FileService is the object store Source Files are kept in (ADR-0006). It
// works in object keys, never bucket names or provider schemes, so the
// bucket or vendor can change without rewriting stored keys.
type FileService interface {
	// CheckConnectivity verifies that the storage backend is reachable and
	// that its bucket exists. It never creates anything.
	CheckConnectivity(ctx context.Context) error
	// SaveFile stores the Source File of one upload of a Document and
	// returns its object key.
	SaveFile(ctx context.Context, file *multipart.FileHeader, documentID, uploadID uuid.UUID) (string, error)
	// GetFile opens the object at key. A missing object is ErrFileNotFound.
	GetFile(ctx context.Context, key string) (io.ReadCloser, error)
	// DeleteFile deletes the object at key; deleting a missing one is not an
	// error.
	DeleteFile(ctx context.Context, key string) error
}

// ErrFileNotFound is returned by GetFile when no object exists at the key.
var ErrFileNotFound = errors.New("file not found")
