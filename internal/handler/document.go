// Copyright (C) 2026 Tencent. All rights reserved.
// handleDuplicateDocumentError, parseCommaSeparated, isRequestBodyTooLarge
// and uploadEnvelopeSlack are adapted from WeKnora
// (https://github.com/Tencent/WeKnora), MIT License, by way of Omnisc
// (internal/handler/knowledge.go and upload_limit.go). Modified for Trelis:
// net/http instead of gin, Documents instead of Knowledge, and no per-step
// logging (Trelis logs only failures, through internal/logger). See NOTICE.

package handler

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/logger"
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
	// The ingest event stream timings. Zero uses the defaults in
	// ingest_events.go; tests set them through the options below.
	ingestEventPollInterval time.Duration
	ingestEventKeepalive    time.Duration
	ingestEventTimeout      time.Duration
}

// DocumentHandlerOption configures a DocumentHandler. The server uses the
// defaults; tests shorten the ingest event stream timings.
type DocumentHandlerOption func(*DocumentHandler)

// WithIngestEventPollInterval sets how often an open ingest event stream looks
// for new events. A non-positive duration is ignored.
func WithIngestEventPollInterval(d time.Duration) DocumentHandlerOption {
	return func(h *DocumentHandler) {
		if d > 0 {
			h.ingestEventPollInterval = d
		}
	}
}

// WithIngestEventKeepalive sets how long an idle ingest event stream waits
// before writing a keepalive comment. A non-positive duration is ignored.
func WithIngestEventKeepalive(d time.Duration) DocumentHandlerOption {
	return func(h *DocumentHandler) {
		if d > 0 {
			h.ingestEventKeepalive = d
		}
	}
}

// WithIngestEventTimeout caps how long one ingest event stream stays open, so
// a Document stuck in processing cannot hold it forever. A non-positive
// duration is ignored.
func WithIngestEventTimeout(d time.Duration) DocumentHandlerOption {
	return func(h *DocumentHandler) {
		if d > 0 {
			h.ingestEventTimeout = d
		}
	}
}

// NewDocumentHandler creates a new Document handler. Uploads larger than
// maxFileSize bytes are refused. opts set the ingest event stream timings.
func NewDocumentHandler(service interfaces.DocumentService, maxFileSize int64,
	opts ...DocumentHandlerOption,
) *DocumentHandler {
	h := &DocumentHandler{service: service, maxFileSize: maxFileSize}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// CreateDocumentFromFile answers POST /documents: a multipart/form-data body
// with a "file" part, and optional "principals" and "groups" fields, each a
// comma-separated list of names, making up the Access List (the uploader is
// always on it). It answers 200 with the pending Document, or 409 with the
// earlier Document when the caller already uploaded the same content.
//
//	@Summary		Upload a Document
//	@Description	Uploads one file as a new Document. "principals" and "groups" each name a comma-separated list added to the Access List; the uploader is always on it. Answers 409 when the caller already uploaded the same content.
//	@Tags			documents
//	@Accept			multipart/form-data
//	@Produce		json
//	@Security		bearerAuth
//	@Param			file		formData	file	true	"The file to upload"
//	@Param			principals	formData	string	false	"Comma-separated Principal names for the Access List"
//	@Param			groups		formData	string	false	"Comma-separated Group names for the Access List"
//	@Success		200			{object}	handler.DataResponse{data=types.Document}
//	@Failure		400			{object}	handler.ErrorResponse	"Malformed upload, unknown Principal or Group, or unsupported file type"
//	@Failure		401			{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		409			{object}	handler.DuplicateDocumentResponse	"The caller already uploaded this content"
//	@Failure		413			{object}	handler.ErrorResponse	"File larger than storage.max_file_size_mb"
//	@Failure		500			{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/documents [post]
func (h *DocumentHandler) CreateDocumentFromFile(w http.ResponseWriter, r *http.Request) {
	file, ok := h.parseSingleFileUpload(w, r)
	if !ok {
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	accessList := types.AccessList{
		Principals: parseCommaSeparated(r.FormValue("principals")),
		Groups:     parseCommaSeparated(r.FormValue("groups")),
	}
	doc, err := h.service.CreateDocumentFromFile(r.Context(), file, accessList)
	if err != nil {
		if h.handleDuplicateDocumentError(r.Context(), w, err) {
			return
		}
		writeError(w, r, err)
		return
	}
	writeData(r.Context(), w, http.StatusOK, doc)
}

// ReplaceDocumentFile answers PUT /documents/{id}/file: a multipart/form-data
// body with exactly one "file" part replaces the Document's Source File. Only
// the owner may replace it; the Access List stays as it is. It answers 200
// with the pending Document, 409 with the earlier Document when the caller
// already has the same content, or 409 when a concurrent change won.
//
//	@Summary		Replace a Document's Source File
//	@Description	Replaces the Document's Source File with the uploaded one, under a new upload ID, and starts ingesting the new upload. The Access List is unchanged. Only the owner may replace it; the access and owner checks run before the body is read. Answers 409 with the earlier Document when the caller already has a Document with the same content. A concurrent re-upload answers 412; a concurrent delete answers 404. The old Chunks are removed before this returns.
//	@Tags			documents
//	@Accept			multipart/form-data
//	@Produce		json
//	@Security		bearerAuth
//	@Param			id		path		string	true	"Document ID"	format(uuid)
//	@Param			file	formData	file	true	"The new Source File"
//	@Success		200		{object}	handler.DataResponse{data=types.Document}
//	@Failure		400		{object}	handler.ErrorResponse	"Malformed upload, invalid Document ID or unsupported file type"
//	@Failure		401		{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		403		{object}	handler.ErrorResponse	"The caller can read the Document but is not its owner"
//	@Failure		404		{object}	handler.ErrorResponse	"No readable Document with that ID"
//	@Failure		409		{object}	handler.DuplicateDocumentResponse	"The caller already has a Document with this content"
//	@Failure		412		{object}	handler.ErrorResponse	"A concurrent re-upload changed the Document; retry"
//	@Failure		413		{object}	handler.ErrorResponse	"File larger than storage.max_file_size_mb"
//	@Failure		500		{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/documents/{id}/file [put]
func (h *DocumentHandler) ReplaceDocumentFile(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, errors.NewBadRequestError("Invalid Document ID"))
		return
	}
	// Check access and ownership before reading a body the caller may not be
	// allowed to send, so a non-owner gets 403 rather than 400 or 413. The
	// service checks again inside ReplaceDocumentFile.
	if err := h.service.EnsureCanReplace(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	file, ok := h.parseSingleFileUpload(w, r)
	if !ok {
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	doc, err := h.service.ReplaceDocumentFile(r.Context(), id, file)
	if err != nil {
		if h.handleDuplicateDocumentError(r.Context(), w, err) {
			return
		}
		writeError(w, r, err)
		return
	}
	writeData(r.Context(), w, http.StatusOK, doc)
}

// parseSingleFileUpload caps the request body, parses its multipart form and
// returns its one "file" part. A malformed or oversized request is answered
// here and reported as false. The caller removes the form's temporary files
// when it is done.
func (h *DocumentHandler) parseSingleFileUpload(w http.ResponseWriter, r *http.Request) (*multipart.FileHeader, bool) {
	tooLarge := errors.NewRequestEntityTooLargeError("File size cannot exceed " + humanSize(h.maxFileSize))

	// Capped before the multipart parse, not after: ParseMultipartForm
	// buffers the whole body first, so the size check below only ever sees
	// an upload already accepted.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxFileSize+uploadEnvelopeSlack)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		if isRequestBodyTooLarge(err) {
			writeError(w, r, tooLarge)
			return nil, false
		}
		writeError(w, r, errors.NewBadRequestError("File upload failed").WithDetails(err.Error()))
		return nil, false
	}

	files := r.MultipartForm.File["file"]
	if len(files) != 1 {
		writeError(w, r, errors.NewBadRequestError(`File upload failed: send exactly one "file" part`))
		return nil, false
	}
	file := files[0]
	if file.Size > h.maxFileSize {
		writeError(w, r, tooLarge)
		return nil, false
	}
	return file, true
}

// DuplicateDocumentResponse is the 409 body when the caller already uploaded
// the same content: Omnisc's duplicate envelope, with the earlier Document.
type DuplicateDocumentResponse struct {
	Success bool            `json:"success" example:"false"`
	Message string          `json:"message"`
	Data    *types.Document `json:"data"`
	Code    string          `json:"code" enums:"duplicate_document"`
}

// handleDuplicateDocumentError answers 409 with the existing Document, in
// Omnisc's duplicate envelope. It reports whether err was a duplicate.
// Omnisc's code is "duplicate_file"; GLOSSARY.md names the unit a Document.
func (h *DocumentHandler) handleDuplicateDocumentError(ctx context.Context, w http.ResponseWriter, err error) bool {
	dupErr, ok := stderrors.AsType[*types.DuplicateDocumentError](err)
	if !ok {
		return false
	}
	writeJSON(ctx, w, http.StatusConflict, DuplicateDocumentResponse{
		Message: dupErr.Error(),
		Data:    dupErr.Document, // the existing Document
		Code:    "duplicate_document",
	})
	return true
}

// GetDocument answers GET /documents/{id} with the Document, or 404 when it
// does not exist or the caller cannot read it, so a Restricted Document's
// existence is not revealed. Only the owner sees its Access List.
//
//	@Summary		Get a Document
//	@Description	Returns the Document, or 404 when it does not exist or the caller cannot read it, so a Restricted Document's existence is not revealed. Only the owner sees its Access List.
//	@Tags			documents
//	@Produce		json
//	@Security		bearerAuth
//	@Param			id	path	string	true	"Document ID"	format(uuid)
//	@Success		200	{object}	handler.DataResponse{data=types.Document}
//	@Failure		401	{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		404	{object}	handler.ErrorResponse	"No readable Document with that ID"
//	@Failure		500	{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/documents/{id} [get]
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
	writeData(r.Context(), w, http.StatusOK, doc)
}

// DeleteDocument answers DELETE /documents/{id}: only the owner may delete a
// Document. It removes the Document and everything derived from it, cancels
// its ingest and, once the row is gone, removes its objects.
//
//	@Summary		Delete a Document
//	@Description	Only the Document's owner may delete it. Removes the Document and everything derived from it, cancels its ingest and, once the row is gone, removes every object it stored. A reader who is not the owner gets 403; a caller who cannot read the Document gets 404, the same as for one that does not exist.
//	@Tags			documents
//	@Security		bearerAuth
//	@Param			id	path	string	true	"Document ID"	format(uuid)
//	@Success		204	"No Content"
//	@Failure		400	{object}	handler.ErrorResponse	"Invalid Document ID"
//	@Failure		401	{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		403	{object}	handler.ErrorResponse	"The caller can read the Document but is not its owner"
//	@Failure		404	{object}	handler.ErrorResponse	"No readable Document with that ID"
//	@Failure		500	{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/documents/{id} [delete]
func (h *DocumentHandler) DeleteDocument(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, errors.NewBadRequestError("Invalid Document ID"))
		return
	}
	if err := h.service.DeleteDocument(r.Context(), id); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DownloadDocumentFile streams a Document's Source File to a caller who can
// read it, like Omnisc's DownloadKnowledgeFile. It is always an attachment,
// never rendered in the API's origin (ADR-0006).
//
//	@Summary		Download a Document's Source File
//	@Description	Streams the file as an attachment, never rendered in the API's origin. The Content-Disposition header names it and Content-Length gives its size.
//	@Tags			documents
//	@Produce		application/octet-stream
//	@Security		bearerAuth
//	@Param			id	path	string	true	"Document ID"	format(uuid)
//	@Success		200	{file}	file	"The Source File"
//	@Failure		401	{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		404	{object}	handler.ErrorResponse	"No readable Document with that ID"
//	@Failure		500	{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/documents/{id}/file [get]
func (h *DocumentHandler) DownloadDocumentFile(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, errors.NewNotFoundError("Document not found"))
		return
	}
	doc, body, err := h.service.GetDocumentFile(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer func() { _ = body.Close() }()

	contentType := mime.TypeByExtension("." + doc.FileType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": doc.FileName})
	if disposition == "" {
		disposition = "attachment" // a name FormatMediaType cannot encode
	}
	hdr.Set("Content-Disposition", disposition)
	hdr.Set("Content-Length", strconv.FormatInt(doc.FileSize, 10))
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "sandbox")
	hdr.Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		// The status is already sent; the client sees a short body.
		logger.Ctx(r.Context()).Error("stream source file", zap.Stringer("document", doc.ID), zap.Error(err))
	}
}

// ListDocuments answers GET /documents?page=&page_size= with a page of the
// Documents the caller can read, newest first, without Access Lists.
//
//	@Summary		List readable Documents
//	@Description	Returns one page of the Documents the caller can read, newest first, without Access Lists. A page or page_size of 0, or either left out, takes the default: page 1 and page_size 20.
//	@Tags			documents
//	@Produce		json
//	@Security		bearerAuth
//	@Param			page		query	int	false	"Page number, 1-based; 0 or absent means the default of 1"	minimum(0)	maximum(1000000)	default(1)
//	@Param			page_size	query	int	false	"Page size; 0 or absent means the default of 20"	minimum(0)	maximum(1000)	default(20)
//	@Success		200	{object}	handler.PageResponse{data=[]types.Document}
//	@Failure		400	{object}	handler.ErrorResponse	"Invalid page or page_size"
//	@Failure		401	{object}	router.ErrorResponse	"Missing or invalid API key"
//	@Failure		500	{object}	handler.ErrorResponse	"Internal server error; a failed API key check answers the router's error body instead"
//	@Router			/documents [get]
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
	writePage(r.Context(), w, result)
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
