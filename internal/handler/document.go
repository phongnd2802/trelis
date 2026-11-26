package handler

import (
	stderrors "errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/types"
	"github.com/phongnd2802/trelis/internal/types/interfaces"
)

// uploadEnvelopeSlack is the multipart framing allowed on top of the file
// itself, so a body cap refuses a genuinely oversized upload without
// rejecting a legal one for its boundary lines. (From Omnisc's
// upload_limit.go.)
const uploadEnvelopeSlack = 1 << 20

// multipartMemory is how much of a multipart body is held in memory; the
// rest spills to temp files until the request ends.
const multipartMemory = 32 << 20

// DocumentHandler processes HTTP requests related to Documents. It mirrors
// Omnisc's KnowledgeHandler for the file upload, get and list routes; the
// caller is the Principal the router authenticated.
type DocumentHandler struct {
	service     interfaces.DocumentService
	maxFileSize int64
}

// NewDocumentHandler creates a new Document handler. Uploads larger than
// maxFileSize bytes are refused.
func NewDocumentHandler(service interfaces.DocumentService, maxFileSize int64) *DocumentHandler {
	return &DocumentHandler{service: service, maxFileSize: maxFileSize}
}

// CreateDocumentFromFile answers POST /documents: a multipart/form-data body
// with a "file" part, and optional "principals" and "groups" fields, each a
// comma-separated list of names, making up the Access List (the uploader is
// always on it). It answers 200 with the pending Document, or 409 with the
// earlier Document when the caller already uploaded the same content.
func (h *DocumentHandler) CreateDocumentFromFile(w http.ResponseWriter, r *http.Request) {
	tooLarge := errors.NewRequestEntityTooLargeError("File size cannot exceed " + humanSize(h.maxFileSize))

	// Capped before the multipart parse, not after: ParseMultipartForm
	// buffers the whole body first, so the size check below only ever sees
	// an upload already accepted.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxFileSize+uploadEnvelopeSlack)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, r, tooLarge)
			return
		}
		writeError(w, r, errors.NewBadRequestError("File upload failed").WithDetails(err.Error()))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		writeError(w, r, errors.NewBadRequestError(`File upload failed: send exactly one "file" part`))
		return
	}
	file := files[0]
	if file.Size > h.maxFileSize {
		writeError(w, r, tooLarge)
		return
	}

	accessList := types.AccessList{
		Principals: parseCommaSeparated(r.FormValue("principals")),
		Groups:     parseCommaSeparated(r.FormValue("groups")),
	}
	doc, err := h.service.CreateDocumentFromFile(r.Context(), file, accessList)
	if err != nil {
		if h.handleDuplicateDocumentError(w, err, doc) {
			return
		}
		writeError(w, r, err)
		return
	}
	writeData(w, http.StatusOK, doc)
}

// handleDuplicateDocumentError answers 409 with the existing Document, in
// Omnisc's duplicate envelope. It reports whether err was a duplicate.
func (h *DocumentHandler) handleDuplicateDocumentError(w http.ResponseWriter, err error, doc *types.Document) bool {
	dupErr, ok := stderrors.AsType[*types.DuplicateDocumentError](err)
	if !ok {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"success": false,
		"message": dupErr.Error(),
		"data":    doc, // doc is the existing Document
		"code":    "duplicate_file",
	})
	return true
}

// GetDocument answers GET /documents/{id} with the Document, or 404 when it
// does not exist or the caller cannot read it, so a Restricted Document's
// existence is not revealed. Only the owner sees its Access List.
func (h *DocumentHandler) GetDocument(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, errors.NewNotFoundError("Document not found"))
		return
	}
	doc, err := h.service.GetDocumentByID(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeData(w, http.StatusOK, doc)
}

// ListDocuments answers GET /documents?page=&page_size= with a page of the
// Documents the caller can read, newest first, without Access Lists.
func (h *DocumentHandler) ListDocuments(w http.ResponseWriter, r *http.Request) {
	page, err := parsePagination(r)
	if err != nil {
		writeError(w, r, errors.NewBadRequestError(err.Error()))
		return
	}
	result, err := h.service.ListPagedDocuments(r.Context(), page)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writePage(w, result)
}

// parsePagination reads page and page_size from the query string; a missing
// one takes its default.
func parsePagination(r *http.Request) (*types.Pagination, error) {
	var page types.Pagination
	for name, dst := range map[string]*int{"page": &page.Page, "page_size": &page.PageSize} {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer", name)
		}
		*dst = n
	}
	if err := page.Validate(); err != nil {
		return nil, err
	}
	return &page, nil
}

// parseCommaSeparated splits a comma-separated form value into trimmed,
// non-empty names. (Omnisc's parseCommaSeparatedTagIDs.)
func parseCommaSeparated(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		result = append(result, p)
	}
	return result
}

// isRequestBodyTooLarge reports whether a multipart parsing error is the body
// cap firing rather than a malformed or absent part. (From Omnisc's
// upload_limit.go.)
func isRequestBodyTooLarge(err error) bool {
	_, ok := stderrors.AsType[*http.MaxBytesError](err)
	return ok
}

// humanSize names a size limit in MB when it is a whole number of them, else
// in bytes.
func humanSize(bytes int64) string {
	if mb := bytes >> 20; mb > 0 && mb<<20 == bytes {
		return fmt.Sprintf("%dMB", mb)
	}
	return fmt.Sprintf("%d bytes", bytes)
}
