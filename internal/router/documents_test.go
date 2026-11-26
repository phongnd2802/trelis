package router

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/phongnd2802/trelis/internal/auth"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/documents"
)

// server is Trelis on a fresh database, with Principals that each hold one
// API key.
type server struct {
	t    *testing.T
	auth *auth.Store
	h    http.Handler
	keys map[string]string // Principal name → API key
	ctx  context.Context
}

func newServer(t *testing.T, principals ...string) *server {
	t.Helper()
	pool := dbtest.Migrated(t)
	c := &server{t: t, auth: auth.NewStore(pool), keys: map[string]string{}, ctx: context.Background()}
	for _, name := range principals {
		if _, err := c.auth.CreatePrincipal(c.ctx, name); err != nil {
			t.Fatal(err)
		}
		_, key, err := c.auth.CreateKey(c.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		c.keys[name] = key
	}
	c.h = New(Readiness{}, c.auth, documents.NewStore(pool))
	return c
}

// group creates a Group with the given members.
func (c *server) group(name string, members ...string) {
	c.t.Helper()
	if _, err := c.auth.CreateGroup(c.ctx, name); err != nil {
		c.t.Fatal(err)
	}
	for _, m := range members {
		if err := c.auth.AddMember(c.ctx, name, m); err != nil {
			c.t.Fatal(err)
		}
	}
}

// form is one multipart field of an upload; a file field has a filename.
type form struct {
	field, filename, value string
}

func file(name, content string) form   { return form{field: "file", filename: name, value: content} }
func principalField(name string) form  { return form{field: "principal", value: name} }
func groupField(name string) form      { return form{field: "group", value: name} }
func (c *server) key(as string) string { return "Bearer " + c.keys[as] }

// upload POSTs /documents as the named Principal.
func (c *server) upload(as string, fields ...form) *httptest.ResponseRecorder {
	c.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range fields {
		var err error
		if f.filename != "" {
			w, ferr := mw.CreateFormFile(f.field, f.filename)
			if ferr != nil {
				c.t.Fatal(ferr)
			}
			_, err = w.Write([]byte(f.value))
		} else {
			err = mw.WriteField(f.field, f.value)
		}
		if err != nil {
			c.t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		c.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/documents", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", c.key(as))
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	return rec
}

// uploaded uploads and returns the new Document's ID, failing unless 201.
func (c *server) uploaded(as string, fields ...form) string {
	c.t.Helper()
	rec := c.upload(as, fields...)
	if rec.Code != http.StatusCreated {
		c.t.Fatalf("upload as %s = %d %s, want 201", as, rec.Code, rec.Body)
	}
	var doc struct{ ID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.ID == "" {
		c.t.Fatalf("upload body = %s, want a Document with an id", rec.Body)
	}
	return doc.ID
}

type documentBody struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size_bytes"`
	Status      string `json:"status"`
	AccessList  *struct {
		Principals []string `json:"principals"`
		Groups     []string `json:"groups"`
	} `json:"access_list"`
}

func TestAnUploadedDocumentIsPendingAndReadableByItsUploader(t *testing.T) {
	c := newServer(t, "alice")
	id := c.uploaded("alice", file("luat-thue.txt", "Điều 1. Phạm vi điều chỉnh"))

	rec := get(t, c.h, "/documents/"+id, c.key("alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /documents/%s = %d %s, want 200", id, rec.Code, rec.Body)
	}
	var doc documentBody
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ID != id || doc.Filename != "luat-thue.txt" || doc.Status != "pending" ||
		doc.Size != int64(len("Điều 1. Phạm vi điều chỉnh")) || doc.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("document = %+v, want %s luat-thue.txt, pending, its size and a sniffed text type", doc, id)
	}
}

// list GETs /documents as the named Principal and returns the filenames.
func (c *server) list(as string) []string {
	c.t.Helper()
	rec := get(c.t, c.h, "/documents", c.key(as))
	if rec.Code != http.StatusOK {
		c.t.Fatalf("GET /documents as %s = %d %s, want 200", as, rec.Code, rec.Body)
	}
	var body struct{ Documents []documentBody }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		c.t.Fatal(err)
	}
	names := []string{}
	for _, d := range body.Documents {
		names = append(names, d.Filename)
	}
	return names
}

func TestBCannotSeeARestrictedDocumentOfA(t *testing.T) {
	c := newServer(t, "alice", "bob")
	id := c.uploaded("alice", file("luong.txt", "Bảng lương 2024"))

	rec := get(t, c.h, "/documents/"+id, c.key("bob"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("bob GET alice's Document = %d %s, want 404", rec.Code, rec.Body)
	}
	missing := get(t, c.h, "/documents/0190d2a8-0000-7000-8000-00000000dead", c.key("bob"))
	if rec.Body.String() != missing.Body.String() {
		t.Errorf("unreadable body %s differs from missing body %s, which reveals existence", rec.Body, missing.Body)
	}
	if got := c.list("bob"); len(got) != 0 {
		t.Errorf("bob lists %v, want nothing", got)
	}
	if got := c.list("alice"); len(got) != 1 || got[0] != "luong.txt" {
		t.Errorf("alice lists %v, want [luong.txt]", got)
	}
}

func TestTheAccessListGrantsPrincipalsAndGroupMembers(t *testing.T) {
	c := newServer(t, "alice", "bob", "carol", "dave")
	c.group("tax", "carol")
	id := c.uploaded("alice", file("nghi-dinh.txt", "Nghị định 65"), principalField("bob"), groupField("tax"))
	c.uploaded("alice", file("private.txt", "only alice"))

	for who, want := range map[string]int{"alice": http.StatusOK, "bob": http.StatusOK, "carol": http.StatusOK, "dave": http.StatusNotFound} {
		if rec := get(t, c.h, "/documents/"+id, c.key(who)); rec.Code != want {
			t.Errorf("%s GET = %d, want %d", who, rec.Code, want)
		}
	}
	if got := c.list("carol"); len(got) != 1 || got[0] != "nghi-dinh.txt" {
		t.Errorf("carol lists %v, want [nghi-dinh.txt]", got)
	}
	if got := c.list("alice"); len(got) != 2 {
		t.Errorf("alice lists %v, want both of her uploads", got)
	}

}

func TestOnlyTheOwnerSeesTheAccessList(t *testing.T) {
	c := newServer(t, "alice", "bob", "carol")
	c.group("tax", "carol")
	id := c.uploaded("alice", file("nghi-dinh.txt", "Nghị định 65"), principalField("bob"), groupField("tax"))

	doc := func(as string) documentBody {
		t.Helper()
		var d documentBody
		rec := get(t, c.h, "/documents/"+id, c.key(as))
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	got := doc("alice").AccessList
	if got == nil || len(got.Principals) != 2 || got.Principals[0] != "alice" ||
		got.Principals[1] != "bob" || len(got.Groups) != 1 || got.Groups[0] != "tax" {
		t.Errorf("alice sees access list %+v, want principals [alice bob], groups [tax]", got)
	}
	for _, reader := range []string{"bob", "carol"} {
		if got := doc(reader).AccessList; got != nil {
			t.Errorf("%s sees access list %+v, want none", reader, got)
		}
	}
}

func TestJoiningAGroupGrantsItsDocumentsOnTheNextRequest(t *testing.T) {
	c := newServer(t, "alice", "bob")
	c.group("legal")
	c.uploaded("alice", file("luat.txt", "Luật 04/2007"), groupField("legal"))
	if got := c.list("bob"); len(got) != 0 {
		t.Fatalf("bob before joining lists %v, want nothing", got)
	}
	if err := c.auth.AddMember(c.ctx, "legal", "bob"); err != nil {
		t.Fatal(err)
	}
	if got := c.list("bob"); len(got) != 1 {
		t.Errorf("bob after joining lists %v, want [luat.txt]", got)
	}
}

func TestAnUploadNamingAnUnknownReaderStoresNothing(t *testing.T) {
	c := newServer(t, "alice")
	for _, f := range []form{principalField("ghost"), groupField("nobody")} {
		if rec := c.upload("alice", file("a.txt", "x"), f); rec.Code != http.StatusBadRequest {
			t.Errorf("upload with %s %q = %d %s, want 400", f.field, f.value, rec.Code, rec.Body)
		}
	}
	if got := c.list("alice"); len(got) != 0 {
		t.Errorf("alice lists %v after failed uploads, want nothing", got)
	}
}

func TestMalformedUploadsAre400(t *testing.T) {
	c := newServer(t, "alice")
	for name, fields := range map[string][]form{
		"no file":         {principalField("alice")},
		"empty file":      {file("a.txt", "")},
		"two files":       {file("a.txt", "x"), file("b.txt", "y")},
		"unknown field":   {file("a.txt", "x"), {field: "principals", value: "alice"}},
		"empty principal": {file("a.txt", "x"), principalField("")},
	} {
		if rec := c.upload("alice", fields...); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: upload = %d %s, want 400", name, rec.Code, rec.Body)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/documents", bytes.NewBufferString(`{"file":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.key("alice"))
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("JSON upload = %d, want 400", rec.Code)
	}
}

func TestAFileOverTheLimitIs413(t *testing.T) {
	c := newServer(t, "alice")
	rec := c.upload("alice", file("big.bin", string(make([]byte, MaxUploadBytes+1))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("upload of %d bytes = %d %s, want 413", MaxUploadBytes+1, rec.Code, rec.Body)
	}
}

func TestAMalformedDocumentIDIs404(t *testing.T) {
	c := newServer(t, "alice")
	if rec := get(t, c.h, "/documents/not-a-uuid", c.key("alice")); rec.Code != http.StatusNotFound {
		t.Errorf("GET /documents/not-a-uuid = %d, want 404", rec.Code)
	}
}

func TestDocumentsNeedAKey(t *testing.T) {
	c := newServer(t)
	if rec := get(t, c.h, "/documents", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /documents without a key = %d, want 401", rec.Code)
	}
}
