//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// principalKey creates a Principal with one API key and returns the key.
func (tr *trelis) principalKey(t *testing.T, name string) string {
	t.Helper()
	tr.admin(t, "principal", "create", name)
	_, key, ok := strings.Cut(tr.admin(t, "key", "create", name), "\t")
	if !ok {
		t.Fatalf("key create for %s printed no key", name)
	}
	return key
}

// upload POSTs one text file to /documents and returns the new Document's ID.
func (tr *trelis) upload(t *testing.T, key, name, content string) string {
	t.Helper()
	return tr.uploadBytes(t, key, name, []byte(content))
}

// uploadBytes POSTs one file with arbitrary bytes to /documents and returns
// the new Document's ID.
func (tr *trelis) uploadBytes(t *testing.T, key, name string, content []byte) string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, tr.url+"/documents", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.Data.ID == "" {
		t.Fatalf("upload %s = %d (%v), want 200 with a Document", name, resp.StatusCode, err)
	}
	return out.Data.ID
}

// documentView is the part of a Document the ingest tests read.
type documentView struct {
	ID          string `json:"id"`
	ParseStatus string `json:"parse_status"`
	ParseError  string `json:"parse_error"`
}

// waitForIngest polls GET /documents/{id} until ingest reaches a final
// status, and returns the Document.
func (tr *trelis) waitForIngest(t *testing.T, key, id string) documentView {
	t.Helper()
	const timeout = 60 * time.Second
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequest(http.MethodGet, tr.url+"/documents/"+id, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /documents/%s: %v", id, err)
		}
		var out struct {
			Data documentView `json:"data"`
		}
		err = json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /documents/%s = %d (%v), want 200 with the Document", id, resp.StatusCode, err)
		}
		switch out.Data.ParseStatus {
		case "completed", "failed":
			return out.Data
		}
		if time.Now().After(deadline) {
			t.Fatalf("Document %s still %q after %v, want completed or failed", id, out.Data.ParseStatus, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// objects lists the keys in this Trelis's bucket, straight from silo.
func (tr *trelis) objects(t *testing.T) []string {
	t.Helper()
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(deps.s3),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(s3User, s3Password, ""),
	})
	out, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String(tr.bucket)})
	if err != nil {
		t.Fatalf("list bucket %s: %v", tr.bucket, err)
	}
	keys := []string{}
	for _, o := range out.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	return keys
}

func TestAnUploadedSourceFileIsStoredInTheBucketAndDownloadable(t *testing.T) {
	tr := start(t)
	alice := tr.principalKey(t, "alice")
	bob := tr.principalKey(t, "bob")
	content := "Điều 1. Phạm vi điều chỉnh\n"
	id := tr.upload(t, alice, "luat.txt", content)

	keys := tr.objects(t)
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "documents/"+id+"/") || !strings.HasSuffix(keys[0], "/source") {
		t.Errorf("bucket holds %v, want one documents/%s/{upload_id}/source", keys, id)
	}

	resp := tr.get(t, "/documents/"+id+"/file", alice)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(got) != content {
		t.Errorf("alice GET file = %d %q, want 200 with the uploaded bytes", resp.StatusCode, got)
	}
	if d := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(d, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", d)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("headers = %v, want nosniff and a sandbox CSP", resp.Header)
	}
	if resp := tr.get(t, "/documents/"+id+"/file", bob); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob GET file = %d, want 404", resp.StatusCode)
	}
}

// readiness GETs /readyz and returns its status code and per-dependency
// statuses.
func (tr *trelis) readiness(t *testing.T) (int, map[string]string) {
	t.Helper()
	resp := tr.get(t, "/readyz", "")
	var ready struct {
		Checks map[string]struct{ Status string } `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for dep, c := range ready.Checks {
		statuses[dep] = c.Status
	}
	return resp.StatusCode, statuses
}

func TestReadinessSeesTheBucket(t *testing.T) {
	tr := start(t)
	if _, checks := tr.readiness(t); checks["storage"] != "up" {
		t.Errorf("storage = %q, want up", checks["storage"])
	}
}

func TestAMissingBucketFailsReadinessWhenNotCreatedOnStart(t *testing.T) {
	tr := startWith(t, "TRELIS_STORAGE_S3_CREATE_BUCKET=false")
	if code, checks := tr.readiness(t); code != http.StatusServiceUnavailable || checks["storage"] == "up" {
		t.Errorf("readyz = %d, storage %q; want 503 with storage down", code, checks["storage"])
	}
}

func TestTrelisServesWhileObjectStorageIsUnreachable(t *testing.T) {
	tr := startWith(t, "TRELIS_STORAGE_S3_ENDPOINT=http://127.0.0.1:1")
	if code, checks := tr.readiness(t); code != http.StatusServiceUnavailable || checks["storage"] == "up" || checks["postgres"] != "up" {
		t.Errorf("readyz = %d %v; want 503 with storage down and postgres up", code, checks)
	}
}
