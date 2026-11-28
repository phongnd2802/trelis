// Package filetest holds an in-memory FileService for tests, so service and
// router tests need no object store. It is test support only; production
// code must not import it.
package filetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/service/file"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// object is one stored object and when it was last written.
type object struct {
	data    []byte
	modTime time.Time
}

// Memory is a FileService that keeps objects in a map. It uses the same
// key layout as the S3 provider.
type Memory struct {
	mu      sync.Mutex
	objects map[string]object
	// FailSave, when set, is returned by the next SaveFile instead of
	// storing anything.
	FailSave error
}

var _ interfaces.FileService = (*Memory)(nil)

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{objects: map[string]object{}} }

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
	m.objects[key] = object{data: data, modTime: time.Now()}
	return key, nil
}

// GetFile returns a copy of the object at key.
func (m *Memory) GetFile(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return nil, interfaces.ErrFileNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(obj.data))), nil
}

// DeleteFile removes the object at key, if any.
func (m *Memory) DeleteFile(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// ListObjects returns the objects under prefix, sorted by key.
func (m *Memory) ListObjects(_ context.Context, prefix string) ([]interfaces.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var objects []interfaces.ObjectInfo
	for _, key := range slices.Sorted(maps.Keys(m.objects)) {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, interfaces.ObjectInfo{Key: key, LastModified: m.objects[key].modTime})
		}
	}
	return objects, nil
}

// DeletePrefix removes every object under prefix.
func (m *Memory) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			delete(m.objects, key)
		}
	}
	return nil
}

// PutObject stores data at key with the given last-modified time, so a test
// can create an object the normal upload path would not, such as one under a
// key that does not parse.
func (m *Memory) PutObject(key string, data []byte, modTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = object{data: bytes.Clone(data), modTime: modTime}
}

// SetModTime sets the last-modified time of the object at key; a key with no
// object is ignored. It lets a test age an object without waiting.
func (m *Memory) SetModTime(key string, modTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		return
	}
	obj.modTime = modTime
	m.objects[key] = obj
}

// Keys lists the stored keys, sorted.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Sorted(maps.Keys(m.objects))
}
