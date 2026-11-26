// Package filetest holds an in-memory FileService for tests, so service and
// router tests need no object store. Build tag is intentionally absent, as
// in modeltest and dbtest: keep production code from importing it.
package filetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/service/file"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// Memory is a FileService that keeps objects in a map. It uses the same
// key layout as the S3 provider.
type Memory struct {
	mu      sync.Mutex
	objects map[string][]byte
	// FailSave, when set, is returned by the next SaveFile instead of
	// storing anything.
	FailSave error
}

var _ interfaces.FileService = (*Memory)(nil)

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{objects: map[string][]byte{}} }

// CheckConnectivity always succeeds.
func (m *Memory) CheckConnectivity(context.Context) error { return nil }

// SaveFile stores the upload under file.SourceKey.
func (m *Memory) SaveFile(_ context.Context, fh *multipart.FileHeader, documentID, uploadID uuid.UUID) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.FailSave; err != nil {
		m.FailSave = nil
		return "", err
	}
	src, err := fh.Open()
	if err != nil {
		return "", fmt.Errorf("open upload: %w", err)
	}
	defer func() { _ = src.Close() }()
	data, err := io.ReadAll(src)
	if err != nil {
		return "", fmt.Errorf("read upload: %w", err)
	}
	key := file.SourceKey(documentID, uploadID)
	m.objects[key] = data
	return key, nil
}

// GetFile returns a copy of the object at key.
func (m *Memory) GetFile(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[key]
	if !ok {
		return nil, interfaces.ErrFileNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), nil
}

// DeleteFile removes the object at key, if any.
func (m *Memory) DeleteFile(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// Keys lists the stored keys, sorted.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.objects))
}
