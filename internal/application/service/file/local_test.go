package file

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// fileHeader builds the *multipart.FileHeader an upload handler would get.
func fileHeader(t *testing.T, name, content string) *multipart.FileHeader {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	form, err := multipart.NewReader(&body, mw.Boundary()).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = form.RemoveAll() })
	return form.File["file"][0]
}

func TestLocalSavesReadsAndDeletesAFile(t *testing.T) {
	ctx := context.Background()
	svc := NewLocalFileService(t.TempDir())
	if err := svc.CheckConnectivity(ctx); err != nil {
		t.Fatalf("CheckConnectivity: %v", err)
	}

	path, err := svc.SaveFile(ctx, fileHeader(t, "luat.pdf", "%PDF-1.7"), uuid.New())
	if err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	if filepath.Ext(path) != ".pdf" || path[:len(localScheme)] != localScheme {
		t.Errorf("path = %q, want local://….pdf", path)
	}
	rc, err := svc.GetFile(ctx, path)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "%PDF-1.7" {
		t.Errorf("content = %q, want what was saved", got)
	}
	if err := svc.DeleteFile(ctx, path); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := svc.GetFile(ctx, path); err == nil {
		t.Error("GetFile after DeleteFile succeeded, want an error")
	}
}

func TestLocalRefusesPathsOutsideItsBase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "files")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	svc := NewLocalFileService(base)
	for _, path := range []string{"local://../secret", filepath.Join(root, "secret"), "s3://bucket/secret"} {
		if _, err := svc.GetFile(ctx, path); err == nil {
			t.Errorf("GetFile(%q) succeeded, want refused", path)
		}
	}
}

func TestLocalReportsAMissingBaseDirectory(t *testing.T) {
	svc := NewLocalFileService(filepath.Join(t.TempDir(), "missing"))
	if err := svc.CheckConnectivity(context.Background()); err == nil {
		t.Error("CheckConnectivity on a missing directory succeeded, want an error")
	}
}
