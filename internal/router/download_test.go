package router

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"testing"
)

func TestTheUploaderDownloadsTheSourceFileAsAnAttachment(t *testing.T) {
	c := newServer(t, "alice")
	content := "Điều 1. Phạm vi điều chỉnh"
	id := c.uploaded("alice", upFile("luật thuế.txt", content))

	rec := get(t, c.h, "/documents/"+id+"/file", c.key("alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET file = %d %s, want 200", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != content {
		t.Errorf("body = %q, want the uploaded bytes %q", got, content)
	}
	disp, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
	if err != nil || disp != "attachment" || params["filename"] != "luật thuế.txt" {
		t.Errorf("Content-Disposition = %q (%v), want attachment with the uploaded name", rec.Header().Get("Content-Disposition"), err)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox",
		"Content-Length":          fmt.Sprint(len(content)),
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain for a .txt Source File", ct)
	}
}

func TestTheSourceFileIsStoredUnderItsDocumentAndUploadKey(t *testing.T) {
	c := newServer(t, "alice")
	id := c.uploaded("alice", upFile("a.txt", "x"))
	keys := c.files.Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "documents/"+id+"/") || !strings.HasSuffix(keys[0], "/source") {
		t.Errorf("stored keys = %v, want one documents/%s/{upload_id}/source", keys, id)
	}
}

func TestAPrincipalOffTheAccessListGetsTheSame404AsForAMissingDocument(t *testing.T) {
	c := newServer(t, "alice", "bob")
	id := c.uploaded("alice", upFile("mat.txt", "restricted"))

	denied := get(t, c.h, "/documents/"+id+"/file", c.key("bob"))
	missing := get(t, c.h, "/documents/00000000-0000-0000-0000-000000000000/file", c.key("bob"))
	malformed := get(t, c.h, "/documents/not-a-uuid/file", c.key("bob"))
	for name, rec := range map[string]int{"denied": denied.Code, "missing": missing.Code, "malformed": malformed.Code} {
		if rec != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", name, rec)
		}
	}
	if denied.Body.String() != missing.Body.String() {
		t.Errorf("denied body %s differs from missing body %s", denied.Body, missing.Body)
	}
	if strings.Contains(denied.Body.String(), "restricted") {
		t.Errorf("denied body leaks the content: %s", denied.Body)
	}
}

func TestAGroupMemberOnTheAccessListCanDownload(t *testing.T) {
	c := newServer(t, "alice", "bob")
	c.group("tax", "bob")
	id := c.uploaded("alice", upFile("a.txt", "for tax"), groups("tax"))
	if rec := get(t, c.h, "/documents/"+id+"/file", c.key("bob")); rec.Code != http.StatusOK || rec.Body.String() != "for tax" {
		t.Errorf("bob GET file = %d %q, want 200 with the bytes", rec.Code, rec.Body)
	}
}

func TestAFailedSaveStoresNoDocument(t *testing.T) {
	c := newServer(t, "alice")
	c.files.FailSave = errors.New("bucket unreachable")
	if rec := c.upload("alice", upFile("a.txt", "x")); rec.Code != http.StatusInternalServerError {
		t.Errorf("upload with failing storage = %d %s, want 500", rec.Code, rec.Body)
	}
	if got := c.list("alice"); len(got) != 0 {
		t.Errorf("alice lists %v after a failed save, want nothing", got)
	}
}

func TestDownloadNeedsAKey(t *testing.T) {
	c := newServer(t, "alice")
	id := c.uploaded("alice", upFile("a.txt", "x"))
	if rec := get(t, c.h, "/documents/"+id+"/file", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET file without a key = %d, want 401", rec.Code)
	}
}

func TestARejectedUploadLeavesNoObjectBehind(t *testing.T) {
	c := newServer(t, "alice")
	if rec := c.upload("alice", upFile("a.txt", "x"), principals("ghost")); rec.Code != http.StatusBadRequest {
		t.Fatalf("upload naming ghost = %d %s, want 400", rec.Code, rec.Body)
	}
	if keys := c.files.Keys(); len(keys) != 0 {
		t.Errorf("stored keys after a rejected upload = %v, want none", keys)
	}
}
