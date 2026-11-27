package handler

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"strings"

	"github.com/phongnd2802/trelis/internal/errors"
)

// jsonBodyLimit bounds a POST body that carries one small JSON request, such
// as /retrieve and /answer. Anything larger is a client mistake worth
// refusing before it is decoded.
const jsonBodyLimit = 64 << 10

// unknownJSONFieldPrefix is how encoding/json reports a field the struct does
// not declare when DisallowUnknownFields is set.
const unknownJSONFieldPrefix = `json: unknown field "`

// decodeJSONRequest decodes the one JSON object a POST body must hold into T.
// A body that is not JSON, not an object, or holds more than one object is an
// ErrBadRequest; an unknown field or a field of the wrong type is a
// validation error, the same kind of client mistake as a bad value.
func decodeJSONRequest[T any](r *http.Request) (*T, *errors.AppError) {
	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, requestBodyError(err)
	}
	// The one-object check stays a bad request whatever the second value is.
	if err := dec.Decode(&struct{}{}); !stderrors.Is(err, io.EOF) {
		if isRequestBodyTooLarge(err) {
			return nil, errors.NewRequestEntityTooLargeError("Request body cannot exceed 64KB")
		}
		return nil, errors.NewBadRequestError("Invalid request body").
			WithDetails("the body must hold exactly one JSON object")
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.NewBadRequestError("Invalid request body").
			WithDetails("the body must be a JSON object")
	}

	var req T
	obj := json.NewDecoder(bytes.NewReader(raw))
	// A typo in a field name must be an error, not a silently ignored field
	// that changes what runs.
	obj.DisallowUnknownFields()
	if err := obj.Decode(&req); err != nil {
		return nil, requestBodyError(err)
	}
	return &req, nil
}

// requestBodyError maps a decoding failure to the status its kind deserves.
func requestBodyError(err error) *errors.AppError {
	if isRequestBodyTooLarge(err) {
		return errors.NewRequestEntityTooLargeError("Request body cannot exceed 64KB")
	}
	if isRequestBodyValidationError(err) {
		return errors.NewValidationError("Invalid request body").WithDetails(err.Error())
	}
	return errors.NewBadRequestError("Invalid request body").WithDetails(err.Error())
}

// isRequestBodyValidationError reports whether err is a well-formed JSON body
// with a field or value the request does not accept.
func isRequestBodyValidationError(err error) bool {
	if strings.HasPrefix(err.Error(), unknownJSONFieldPrefix) {
		return true
	}
	var typeErr *json.UnmarshalTypeError
	return stderrors.As(err, &typeErr)
}
