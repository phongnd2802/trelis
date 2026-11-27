package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/application/repository"
	"github.com/phongnd2802/trelis/internal/application/service"
	"github.com/phongnd2802/trelis/internal/application/service/file/filetest"
	"github.com/phongnd2802/trelis/internal/db"
	"github.com/phongnd2802/trelis/internal/db/dbtest"
	"github.com/phongnd2802/trelis/internal/handler"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// testMaxFileSize keeps the over-the-limit test small.
const testMaxFileSize = 1 << 10

// server is Trelis on a fresh database and file store, with Principals that
// each hold one API key.
type server struct {
	t          *testing.T
	principals interfaces.PrincipalService
	h          http.Handler
	// files is the object store the server writes Source Files to.
	files *filetest.Memory
	// ingest records the Documents uploads asked the ingest workflow to
	// process.
	ingest *fakeStarter
	keys   map[string]string // Principal name → API key
	ctx    context.Context
}

// fakeStarter is an IngestStarter that records the IDs it was asked to start
// and can fail.
type fakeStarter struct {
	mu      sync.Mutex
	started []uuid.UUID
	err     error
}

func (f *fakeStarter) StartIngest(_ context.Context, documentID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, documentID)
	return f.err
}

// startedIDs returns the IDs StartIngest was called with, in order.
func (f *fakeStarter) startedIDs() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.started...)
}

func newServer(t *testing.T, principals ...string) *server {
	t.Helper()
	store := db.NewStore(dbtest.Migrated(t))
	principalRepo := repository.NewPrincipalRepository(store)
	authn, principalSvc := realAuth(store)
	c := &server{t: t, principals: principalSvc, keys: map[string]string{}, ctx: context.Background()}
	for _, name := range principals {
		if _, err := c.principals.CreatePrincipal(c.ctx, name); err != nil {
			t.Fatal(err)
		}
		_, key, err := c.principals.CreateAPIKey(c.ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		c.keys[name] = key
	}
	c.files = filetest.NewMemory()
	c.ingest = &fakeStarter{}
	docs := service.NewDocumentService(store,
		repository.NewDocumentRepository(store), principalRepo, c.files, c.ingest)
	c.h = New(Readiness{}, authn, handler.NewPrincipalHandler(principalSvc),
		handler.NewDocumentHandler(docs, testMaxFileSize), nil, nil, nil, false)
	return c
}

// realAuth is the Auth and Principal services on store.
func realAuth(store db.Store) (interfaces.AuthService, interfaces.PrincipalService) {
	principals, keys := repository.NewPrincipalRepository(store), repository.NewAPIKeyRepository(store)
	return service.NewAuthService(principals, keys), service.NewPrincipalService(store, principals, keys)
}

// group creates a Group with the given members.
func (c *server) group(name string, members ...string) {
	c.t.Helper()
	if _, err := c.principals.CreateGroup(c.ctx, name); err != nil {
		c.t.Fatal(err)
	}
	for _, m := range members {
		if err := c.principals.AddMember(c.ctx, name, m); err != nil {
			c.t.Fatal(err)
		}
	}
}

// form is one multipart field of an upload; a file field has a filename.
type form struct {
	field, filename, value string
}

func upFile(name, content string) form { return form{field: "file", filename: name, value: content} }
func principals(names ...string) form {
	return form{field: "principals", value: strings.Join(names, ",")}
}
func groups(names ...string) form      { return form{field: "groups", value: strings.Join(names, ",")} }
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

type documentBody struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	FileName    string `json:"file_name"`
	FileType    string `json:"file_type"`
	FileSize    int64  `json:"file_size"`
	FileHash    string `json:"file_hash"`
	ParseStatus string `json:"parse_status"`
	AccessList  *struct {
		Principals []string `json:"principals"`
		Groups     []string `json:"groups"`
	} `json:"access_list"`
}

// envelope is Omnisc's response shape: {"success", "data"} on success,
// {"success": false, "error": {"code", "message"}} on failure.
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Total    int64  `json:"total"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
	Code     string `json:"code"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %s is not a JSON envelope: %v", rec.Body, err)
	}
	return e
}

func decodeDoc(t *testing.T, rec *httptest.ResponseRecorder) documentBody {
	t.Helper()
	var d documentBody
	if err := json.Unmarshal(decode(t, rec).Data, &d); err != nil {
		t.Fatalf("data in %s is not a Document: %v", rec.Body, err)
	}
	return d
}

// uploaded uploads and returns the new Document's ID, failing unless 200.
func (c *server) uploaded(as string, fields ...form) string {
	c.t.Helper()
	rec := c.upload(as, fields...)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("upload as %s = %d %s, want 200", as, rec.Code, rec.Body)
	}
	d := decodeDoc(c.t, rec)
	if d.ID == "" {
		c.t.Fatalf("upload body = %s, want a Document with an id", rec.Body)
	}
	return d.ID
}

// list GETs /documents as the named Principal and returns the file names.
func (c *server) list(as string) []string {
	c.t.Helper()
	rec := get(c.t, c.h, "/documents", c.key(as))
	if rec.Code != http.StatusOK {
		c.t.Fatalf("GET /documents as %s = %d %s, want 200", as, rec.Code, rec.Body)
	}
	var docs []documentBody
	if err := json.Unmarshal(decode(c.t, rec).Data, &docs); err != nil {
		c.t.Fatal(err)
	}
	names := []string{}
	for _, d := range docs {
		names = append(names, d.FileName)
	}
	return names
}

func TestAnUploadedDocumentIsPendingAndReadableByItsUploader(t *testing.T) {
	c := newServer(t, "alice")
	id := c.uploaded("alice", upFile("luat-thue.txt", "Điều 1. Phạm vi điều chỉnh"))

	rec := get(t, c.h, "/documents/"+id, c.key("alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /documents/%s = %d %s, want 200", id, rec.Code, rec.Body)
	}
	if e := decode(t, rec); !e.Success {
		t.Errorf("success = false in %s", rec.Body)
	}
	doc := decodeDoc(t, rec)
	// sha256sum of the content, computed outside Go.
	const wantHash = "1546822a3c16355554bf80570b4053096016bcdf49926cf1a5246608fa6f845f"
	if doc.ID != id || doc.FileName != "luat-thue.txt" || doc.Title != "luat-thue.txt" ||
		doc.FileType != "txt" || doc.ParseStatus != "pending" ||
		doc.FileSize != int64(len("Điều 1. Phạm vi điều chỉnh")) || doc.FileHash != wantHash {
		t.Errorf("document = %+v, want %s luat-thue.txt, txt, pending, its size and SHA-256", doc, id)
	}
}

func TestBCannotSeeARestrictedDocumentOfA(t *testing.T) {
	c := newServer(t, "alice", "bob")
	id := c.uploaded("alice", upFile("luong.txt", "Bảng lương 2024"))

	rec := get(t, c.h, "/documents/"+id, c.key("bob"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("bob GET alice's Document = %d %s, want 404", rec.Code, rec.Body)
	}
	if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != 1003 {
		t.Errorf("404 body = %s, want the not-found error envelope", rec.Body)
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
	id := c.uploaded("alice", upFile("nghi-dinh.txt", "Nghị định 65"), principals("bob"), groups("tax"))
	c.uploaded("alice", upFile("private.txt", "only alice"))

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
	c.group("legal")
	id := c.uploaded("alice", upFile("nghi-dinh.txt", "Nghị định 65"), principals("bob"), groups(" tax ", "legal", ""))

	doc := func(as string) documentBody {
		t.Helper()
		return decodeDoc(t, get(t, c.h, "/documents/"+id, c.key(as)))
	}
	got := doc("alice").AccessList
	if got == nil || strings.Join(got.Principals, ",") != "alice,bob" || strings.Join(got.Groups, ",") != "legal,tax" {
		t.Errorf("alice sees access list %+v, want principals [alice bob], groups [legal tax]", got)
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
	c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"), groups("legal"))
	if got := c.list("bob"); len(got) != 0 {
		t.Fatalf("bob before joining lists %v, want nothing", got)
	}
	if err := c.principals.AddMember(c.ctx, "legal", "bob"); err != nil {
		t.Fatal(err)
	}
	if got := c.list("bob"); len(got) != 1 {
		t.Errorf("bob after joining lists %v, want [luat.txt]", got)
	}
}

func TestAnUploadStartsTheIngestWorkflowForTheNewDocument(t *testing.T) {
	c := newServer(t, "alice")
	id := c.uploaded("alice", upFile("luat-thue.txt", "Điều 1. Phạm vi điều chỉnh"))

	started := c.ingest.startedIDs()
	if len(started) != 1 || started[0].String() != id {
		t.Errorf("StartIngest called with %v, want once with %s", started, id)
	}
}

func TestAnUploadSucceedsWhenTheIngestWorkflowWillNotStart(t *testing.T) {
	c := newServer(t, "alice")
	c.ingest.err = errors.New("temporal unreachable")

	rec := c.upload("alice", upFile("luat-thue.txt", "Điều 1. Phạm vi điều chỉnh"))
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %s, want 200 even when the workflow did not start", rec.Code, rec.Body)
	}
	if doc := decodeDoc(t, rec); doc.ParseStatus != "pending" {
		t.Errorf("parse_status = %q, want pending", doc.ParseStatus)
	}
	if got := c.ingest.startedIDs(); len(got) != 1 {
		t.Errorf("StartIngest called %d times, want once even though it failed", len(got))
	}
}

func TestAnUploadNamingAnUnknownPrincipalOrGroupStoresNothing(t *testing.T) {
	c := newServer(t, "alice")
	for _, f := range []form{principals("ghost"), groups("nobody")} {
		rec := c.upload("alice", upFile("a.txt", "x"), f)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("upload with %s %q = %d %s, want 400", f.field, f.value, rec.Code, rec.Body)
		}
		if e := decode(t, rec); e.Error == nil || !strings.Contains(e.Error.Message, f.value) {
			t.Errorf("error %s does not name %q", rec.Body, f.value)
		}
	}
	if got := c.list("alice"); len(got) != 0 {
		t.Errorf("alice lists %v after failed uploads, want nothing", got)
	}
}

func TestUploadingTheSameFileTwiceIsAConflict(t *testing.T) {
	c := newServer(t, "alice", "bob")
	id := c.uploaded("alice", upFile("luat.txt", "Luật 04/2007"))

	rec := c.upload("alice", upFile("copy.txt", "Luật 04/2007"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second upload = %d %s, want 409", rec.Code, rec.Body)
	}
	if e := decode(t, rec); e.Code != "duplicate_document" || decodeDoc(t, rec).ID != id {
		t.Errorf("409 body = %s, want code duplicate_document and the existing Document %s", rec.Body, id)
	}
	if got := c.ingest.startedIDs(); len(got) != 1 || got[0].String() != id {
		t.Errorf("StartIngest called with %v after a duplicate upload, want only the first upload's %s", got, id)
	}
	// Another Principal's identical file is their own Document.
	c.uploaded("bob", upFile("luat.txt", "Luật 04/2007"))
}

func TestMalformedUploadsAre400(t *testing.T) {
	c := newServer(t, "alice")
	for name, fields := range map[string][]form{
		"no file":          {principals("alice")},
		"empty file":       {upFile("a.txt", "")},
		"unsupported type": {upFile("setup.exe", "MZ")},
		"no extension":     {upFile("README", "x")},
	} {
		rec := c.upload("alice", fields...)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: upload = %d %s, want 400", name, rec.Code, rec.Body)
			continue
		}
		if e := decode(t, rec); e.Success || e.Error == nil || e.Error.Code != 1000 {
			t.Errorf("%s: body = %s, want the bad-request error envelope", name, rec.Body)
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
	rec := c.upload("alice", upFile("big.txt", strings.Repeat("x", testMaxFileSize+1)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("upload of %d bytes = %d %s, want 413", testMaxFileSize+1, rec.Code, rec.Body)
	}
	if got := c.list("alice"); len(got) != 0 {
		t.Errorf("alice lists %v after a rejected upload, want nothing", got)
	}
}

func TestListingIsPaged(t *testing.T) {
	c := newServer(t, "alice")
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		c.uploaded("alice", upFile(name, name))
	}
	rec := get(t, c.h, "/documents?page=2&page_size=2", c.key("alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET page 2 = %d %s", rec.Code, rec.Body)
	}
	e := decode(t, rec)
	var docs []documentBody
	if err := json.Unmarshal(e.Data, &docs); err != nil {
		t.Fatal(err)
	}
	// Newest first, so page 2 of size 2 holds the oldest upload.
	if e.Total != 3 || e.Page != 2 || e.PageSize != 2 || len(docs) != 1 || docs[0].FileName != "a.txt" {
		t.Errorf("page 2 = total %d page %d size %d %v, want total 3, page 2, size 2, [a.txt]", e.Total, e.Page, e.PageSize, docs)
	}
	for _, q := range []string{"page=-1", "page=x", "page=99999999999", "page_size=1001"} {
		if rec := get(t, c.h, "/documents?"+q, c.key("alice")); rec.Code != http.StatusBadRequest {
			t.Errorf("GET /documents?%s = %d, want 400", q, rec.Code)
		}
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
