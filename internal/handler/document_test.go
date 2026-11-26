package handler

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// stubDocumentService implements just enough of DocumentService to drive
// GetDocument and ListDocuments. Embedding the interface keeps every other
// method nil-panicky on purpose, as Omnisc's handler stubs do.
type stubDocumentService struct {
	interfaces.DocumentService
	getByID func(ctx context.Context, id uuid.UUID) (*types.Document, error)
	list    func(ctx context.Context, page *types.Pagination) (*types.PageResult, error)
}

func (s *stubDocumentService) GetDocumentByID(ctx context.Context, id uuid.UUID) (*types.Document, error) {
	return s.getByID(ctx, id)
}

func (s *stubDocumentService) ListPagedDocuments(ctx context.Context, page *types.Pagination) (*types.PageResult, error) {
	return s.list(ctx, page)
}

func newDocumentTestRouter(svc interfaces.DocumentService) http.Handler {
	h := NewDocumentHandler(svc, 1<<20)
	r := chi.NewRouter()
	r.Get("/documents", h.ListDocuments)
	r.Get("/documents/{id}", h.GetDocument)
	return r
}

func serve(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestAnAppErrorIsAnsweredWithItsEnvelope(t *testing.T) {
	h := newDocumentTestRouter(&stubDocumentService{
		getByID: func(context.Context, uuid.UUID) (*types.Document, error) {
			return nil, errors.NewNotFoundError("Document not found")
		},
	})
	rec := serve(h, "/documents/"+uuid.NewString())
	want := `{"success":false,"error":{"code":1003,"message":"Document not found"}}` + "\n"
	if rec.Code != http.StatusNotFound || rec.Body.String() != want {
		t.Errorf("GET = %d %s, want 404 %s", rec.Code, rec.Body, want)
	}
}

func TestAnInternalErrorIsA500WithoutItsDetail(t *testing.T) {
	h := newDocumentTestRouter(&stubDocumentService{
		list: func(context.Context, *types.Pagination) (*types.PageResult, error) {
			return nil, stderrors.New("pq: password authentication failed for user trelis")
		},
	})
	rec := serve(h, "/documents")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "password") || !strings.Contains(rec.Body.String(), `"code":1007`) {
		t.Errorf("500 body = %s, want the generic internal-error envelope", rec.Body)
	}
}

func TestListDefaultsToTheFirstPageOfTwenty(t *testing.T) {
	var got *types.Pagination
	h := newDocumentTestRouter(&stubDocumentService{
		list: func(_ context.Context, page *types.Pagination) (*types.PageResult, error) {
			got = page
			return types.NewPageResult(0, page, []*types.Document{}), nil
		},
	})
	rec := serve(h, "/documents")
	want := `{"success":true,"data":[],"total":0,"page":1,"page_size":20}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("GET = %d %s, want 200 %s", rec.Code, rec.Body, want)
	}
	if got == nil || got.GetPage() != 1 || got.GetPageSize() != 20 {
		t.Errorf("service got page %+v, want the defaults", got)
	}
}
