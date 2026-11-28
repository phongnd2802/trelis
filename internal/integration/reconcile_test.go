//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/phongnd2802/trelis/internal/application/service/file"
)

// pgConn opens this Trelis's database for a test that seeds state the API
// cannot create.
func (tr *trelis) pgConn(t *testing.T) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPassword, deps.pg.host, deps.pg.port, tr.dbname))
	if err != nil {
		t.Fatalf("connect to database %s: %v", tr.dbname, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// principalID returns the ID of a Principal this Trelis created.
func (tr *trelis) principalID(t *testing.T, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := tr.pgConn(t).QueryRow(context.Background(),
		"SELECT id FROM principals WHERE name = $1", name).Scan(&id)
	if err != nil {
		t.Fatalf("principal %s: %v", name, err)
	}
	return id
}

// putObject writes one object straight into this Trelis's bucket.
func (tr *trelis) putObject(t *testing.T, key string, body []byte) {
	t.Helper()
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(deps.s3),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(s3User, s3Password, ""),
	})
	_, err := client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(tr.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

// TestTheReconcilerIngestsAPendingDocumentAndKeepsAYoungOrphan seeds the
// state CreateDocumentFromFile leaves behind when StartIngest fails: the
// object and the pending Document row, with no workflow. It then waits for
// the reconciler to start ingest and checks that a young orphan prefix is not
// deleted.
func TestTheReconcilerIngestsAPendingDocumentAndKeepsAYoungOrphan(t *testing.T) {
	tr := startWith(t, "TRELIS_RECONCILER_INTERVAL=1s")
	alice := tr.principalKey(t, "alice")
	owner := tr.principalID(t, "alice")

	content := "# Notes\n\nThe launch was in 2024.\n"
	documentID, uploadID := uuid.New(), uuid.New()
	key := file.SourceKey(documentID, uploadID)
	tr.putObject(t, key, []byte(content))
	// Upload writes the object before its row (ADR-0006), so this is the
	// state of an upload in flight: no Document row, and too young to delete.
	orphanDocument, orphanUpload := uuid.New(), uuid.New()
	orphanKey := file.SourceKey(orphanDocument, orphanUpload)
	tr.putObject(t, orphanKey, []byte("not old enough to delete"))

	ctx := context.Background()
	conn := tr.pgConn(t)
	if _, err := conn.Exec(ctx, `
INSERT INTO documents (id, owner_id, title, file_name, file_type, file_size, file_hash, upload_id, object_key)
VALUES ($1, $2, 'notes.md', 'notes.md', 'md', $3, $4, $5, $6)`,
		documentID, owner, int64(len(content)), uuid.NewString(), uploadID, key); err != nil {
		t.Fatalf("insert pending Document: %v", err)
	}
	if _, err := conn.Exec(ctx,
		"INSERT INTO document_principals (document_id, principal_id) VALUES ($1, $2)",
		documentID, owner); err != nil {
		t.Fatalf("grant Document to its owner: %v", err)
	}

	doc := tr.waitForIngest(t, alice, documentID.String())
	if doc.ParseStatus != "completed" {
		t.Fatalf("parse_status = %q (%q), want completed", doc.ParseStatus, doc.ParseError)
	}
	if chunks := tr.chunkContents(t, documentID.String()); len(chunks) == 0 {
		t.Error("no Chunks stored for the Document the reconciler started")
	}
	if keys := tr.objects(t); !slices.Contains(keys, orphanKey) {
		t.Errorf("bucket holds %v, want the young orphan %q kept by the grace period", keys, orphanKey)
	}
}

// TestTheS3StoreListsAndDeletesAPrefix covers the paging and batch delete
// against the real store: only the prefix's objects are listed, a second
// delete is not an error, and objects outside the prefix are untouched.
func TestTheS3StoreListsAndDeletesAPrefix(t *testing.T) {
	tr := startWith(t, "TRELIS_RECONCILER_INTERVAL=1h")
	svc, err := file.NewS3FileService(file.S3Options{
		Endpoint:        deps.s3,
		Region:          "us-east-1",
		Bucket:          tr.bucket,
		AccessKeyID:     s3User,
		SecretAccessKey: s3Password,
		ForcePathStyle:  true,
	})
	if err != nil {
		t.Fatalf("NewS3FileService: %v", err)
	}

	prefix := file.DocumentPrefix(uuid.New())
	first := prefix + uuid.NewString() + "/source"
	second := prefix + uuid.NewString() + "/figures/1.png"
	elsewhere := file.SourceKey(uuid.New(), uuid.New())
	for _, key := range []string{second, elsewhere, first} {
		tr.putObject(t, key, []byte("bytes"))
	}

	ctx := context.Background()
	objects, err := svc.ListObjects(ctx, prefix)
	if err != nil {
		t.Fatalf("ListObjects(%q): %v", prefix, err)
	}
	want := []string{first, second}
	slices.Sort(want)
	got := make([]string, len(objects))
	for i, object := range objects {
		got[i] = object.Key
		if object.LastModified.IsZero() {
			t.Errorf("ListObjects returned %q with no last-modified time", object.Key)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ListObjects(%q) = %v, want %v", prefix, got, want)
	}

	if err := svc.DeletePrefix(ctx, prefix); err != nil {
		t.Fatalf("DeletePrefix(%q): %v", prefix, err)
	}
	if objects, err := svc.ListObjects(ctx, prefix); err != nil || len(objects) != 0 {
		t.Errorf("after DeletePrefix, ListObjects = %v, %v; want nothing", objects, err)
	}
	if err := svc.DeletePrefix(ctx, prefix); err != nil {
		t.Errorf("second DeletePrefix = %v, want no error for an empty prefix", err)
	}
	if objects, err := svc.ListObjects(ctx, elsewhere); err != nil || len(objects) != 1 {
		t.Errorf("object outside the prefix = %v, %v; want it untouched", objects, err)
	}
}
