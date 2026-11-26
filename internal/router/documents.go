package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/phongnd2802/trelis/internal/auth"
	"github.com/phongnd2802/trelis/internal/documents"
)

// Documents stores uploads and answers which Documents a Principal can
// read. Get returns documents.ErrNotFound both for a missing Document and
// for one the Principal cannot read.
type Documents interface {
	Upload(ctx context.Context, owner uuid.UUID, u documents.Upload) (documents.Document, error)
	List(ctx context.Context, principal uuid.UUID) ([]documents.Document, error)
	Get(ctx context.Context, principal, id uuid.UUID) (documents.Document, error)
}

// MaxUploadBytes is the largest file POST /documents accepts.
const MaxUploadBytes = 32 << 20

// formOverhead is what a multipart body may add on top of the file: part
// headers and the Access List fields.
const formOverhead = 1 << 20

// maxFieldBytes bounds one Principal or Group name in an upload form.
const maxFieldBytes = 1 << 10

type documentsAPI struct{ docs Documents }

// upload answers POST /documents, a multipart/form-data body with one
// "file" part and any number of "principal" and "group" fields naming the
// Access List. The uploader is always on it. It answers 201 with the pending
// Document.
func (a documentsAPI) upload(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes+formOverhead)
	u, status, err := readUpload(r)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	doc, err := a.docs.Upload(r.Context(), p.ID, u)
	unknown, isUnknown := errors.AsType[*documents.UnknownNameError](err)
	switch {
	case isUnknown:
		writeError(w, http.StatusBadRequest, "access list: "+unknown.Error())
	case errors.Is(err, documents.ErrEmpty):
		writeError(w, http.StatusBadRequest, "the file is empty or has no name")
	case err != nil:
		slog.ErrorContext(r.Context(), "upload document", "err", err)
		writeError(w, http.StatusInternalServerError, "could not store the document")
	default:
		writeJSON(w, http.StatusCreated, doc)
	}
}

// readUpload parses an upload form, returning the status to answer with
// when it is malformed.
func readUpload(r *http.Request) (documents.Upload, int, error) {
	var u documents.Upload
	mr, err := r.MultipartReader()
	if err != nil {
		return u, http.StatusBadRequest, errors.New("send the file as multipart/form-data")
	}
	seenFile := false
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return u, bodyErrStatus(err), fmt.Errorf("read form: %w", err)
		}
		switch part.FormName() {
		case "file":
			if seenFile {
				return u, http.StatusBadRequest, errors.New("send exactly one file")
			}
			seenFile = true
			if u, err = readFile(part, u); err != nil {
				return u, bodyErrStatus(err), err
			}
		case "principal":
			if u.AccessList.Principals, err = appendField(u.AccessList.Principals, part); err != nil {
				return u, bodyErrStatus(err), err
			}
		case "group":
			if u.AccessList.Groups, err = appendField(u.AccessList.Groups, part); err != nil {
				return u, bodyErrStatus(err), err
			}
		default:
			return u, http.StatusBadRequest, fmt.Errorf("unknown form field %q (want file, principal, group)", part.FormName())
		}
	}
	if !seenFile {
		return u, http.StatusBadRequest, errors.New(`missing the "file" part`)
	}
	return u, 0, nil
}

// errTooLarge is a file over MaxUploadBytes.
var errTooLarge = fmt.Errorf("file is larger than %d bytes", MaxUploadBytes)

func readFile(part *multipart.Part, u documents.Upload) (documents.Upload, error) {
	content, err := io.ReadAll(io.LimitReader(part, MaxUploadBytes+1))
	if err != nil {
		return u, fmt.Errorf("read file: %w", err)
	}
	if len(content) > MaxUploadBytes {
		return u, errTooLarge
	}
	u.Filename = part.FileName()
	u.Content = content
	u.ContentType = part.Header.Get("Content-Type")
	if u.ContentType == "" || u.ContentType == "application/octet-stream" {
		u.ContentType = http.DetectContentType(content)
	}
	return u, nil
}

// appendField reads a Principal or Group name and appends it to names.
func appendField(names []string, part *multipart.Part) ([]string, error) {
	b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
	if err != nil {
		return names, fmt.Errorf("read %s: %w", part.FormName(), err)
	}
	if len(b) > maxFieldBytes {
		return names, fmt.Errorf("%s is longer than %d bytes", part.FormName(), maxFieldBytes)
	}
	if len(b) == 0 {
		return names, fmt.Errorf("%s is empty", part.FormName())
	}
	return append(names, string(b)), nil
}

// bodyErrStatus is 413 when the body went over its limit, else 400.
func bodyErrStatus(err error) int {
	var tooBig *http.MaxBytesError
	if errors.Is(err, errTooLarge) || errors.As(err, &tooBig) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// list answers GET /documents with the Documents the caller can read,
// newest first, without their Access Lists.
func (a documentsAPI) list(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	docs, err := a.docs.List(r.Context(), p.ID)
	if err != nil {
		slog.ErrorContext(r.Context(), "list documents", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list documents")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]documents.Document{"documents": docs})
}

// get answers GET /documents/{id} with the Document (and, for its owner, its
// Access List), or
// 404 when it does not exist or the caller cannot read it, so a Restricted
// Document's existence is not revealed.
func (a documentsAPI) get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.FromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	doc, err := a.docs.Get(r.Context(), p.ID, id)
	switch {
	case errors.Is(err, documents.ErrNotFound):
		writeError(w, http.StatusNotFound, "document not found")
	case err != nil:
		slog.ErrorContext(r.Context(), "get document", "err", err)
		writeError(w, http.StatusInternalServerError, "could not read the document")
	default:
		writeJSON(w, http.StatusOK, doc)
	}
}
