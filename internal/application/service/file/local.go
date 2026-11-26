// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc (internal/application/service/file/local.go).
// Modified for Trelis: files are keyed by Document ID instead of tenant and
// knowledge ID; presigned URLs, copies, temp files, legacy path forms and
// per-step logging are removed. See NOTICE.

// Package file holds the FileService implementations uploaded files are
// stored with.
package file

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/types/interfaces"
	secutils "github.com/phongnd2802/trelis/internal/utils"
)

// localFileService implements the FileService interface for local file system storage
type localFileService struct {
	baseDir string // Base directory for file storage
}

const localScheme = "local://"

// NewLocalFileService creates a new local file service instance storing
// files under baseDir.
func NewLocalFileService(baseDir string) interfaces.FileService {
	return &localFileService{baseDir: baseDir}
}

// CheckConnectivity verifies the local storage directory exists and is accessible.
func (s *localFileService) CheckConnectivity(ctx context.Context) error {
	info, err := os.Stat(s.baseDir)
	if err != nil {
		return fmt.Errorf("storage directory not accessible: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("storage path is not a directory: %s", s.baseDir)
	}
	return nil
}

// SaveFile stores an uploaded file to the local file system
// The file is stored in a directory structure: baseDir/documentID/<unique><ext>
// Returns the provider path (local://...) or an error if saving fails
func (s *localFileService) SaveFile(ctx context.Context,
	file *multipart.FileHeader, documentID uuid.UUID,
) (string, error) {
	// Create storage directory with the Document ID
	dir := filepath.Join(s.baseDir, documentID.String())
	if _, err := secutils.SafePathUnderBase(s.baseDir, dir); err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create directory: %w", err)
	}

	// Generate unique filename using timestamp
	ext := filepath.Ext(file.Filename)
	filename := fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	filePath := filepath.Join(dir, filename)

	// Open source file for reading
	src, err := file.Open()
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = src.Close() }()

	// Create destination file for writing
	dst, err := os.Create(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}

	// Copy content from source to destination
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return "", fmt.Errorf("failed to save file: %w", err)
	}
	if err := dst.Close(); err != nil {
		return "", fmt.Errorf("failed to save file: %w", err)
	}

	// Return provider:// path format: local://{relative_path}
	relPath, _ := filepath.Rel(s.baseDir, filePath)
	return localScheme + filepath.ToSlash(relPath), nil
}

// GetFile retrieves a file from the local file system by its path
// Returns a ReadCloser for reading the file content
// The path must lie under baseDir, so ../../ cannot escape it.
func (s *localFileService) GetFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	resolved, err := s.resolve(filePath)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	return file, nil
}

// DeleteFile removes a file from the local file system
// Returns an error if deletion fails
// The path must lie under baseDir, so ../../ cannot escape it.
func (s *localFileService) DeleteFile(ctx context.Context, filePath string) error {
	resolved, err := s.resolve(filePath)
	if err != nil {
		return err
	}
	if err := os.Remove(resolved); err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}
	return nil
}

// resolve turns a local://{relPath} path into a file path under baseDir.
func (s *localFileService) resolve(filePath string) (string, error) {
	rel, ok := strings.CutPrefix(strings.TrimSpace(filePath), localScheme)
	if !ok {
		return "", fmt.Errorf("invalid file path %q: not a %s path", filePath, localScheme)
	}
	resolved, err := secutils.SafePathUnderBase(s.baseDir, filepath.Join(s.baseDir, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("invalid file path: %w", err)
	}
	return resolved, nil
}
