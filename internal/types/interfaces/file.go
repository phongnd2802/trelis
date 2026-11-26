// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc. Modified for Trelis: no tenants, URLs, temp
// files or copies; files are keyed by Document ID. See NOTICE.

// Package interfaces declares the service, repository and storage contracts
// the handler, service and repository layers meet at.
package interfaces

import (
	"context"
	"io"
	"mime/multipart"

	"github.com/google/uuid"
)

// FileService is where uploaded files are stored. Paths it returns carry a
// provider scheme (e.g. local://...), so another backend can be added
// without changing what is stored on a Document.
type FileService interface {
	// CheckConnectivity verifies that the storage backend is reachable and
	// properly configured.
	CheckConnectivity(ctx context.Context) error
	// SaveFile saves an uploaded file for a Document and returns its path.
	SaveFile(ctx context.Context, file *multipart.FileHeader, documentID uuid.UUID) (string, error)
	// GetFile retrieves a file.
	GetFile(ctx context.Context, filePath string) (io.ReadCloser, error)
	// DeleteFile deletes a file.
	DeleteFile(ctx context.Context, filePath string) error
}
