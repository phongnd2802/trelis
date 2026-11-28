// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc. Modified for Trelis: the common code table is
// kept whole so codes stay stable, the domain codes (tenants, agents, ...)
// and the constructors Trelis does not call are dropped,
// ErrRequestEntityTooLarge, ErrModelProvider and ErrPreconditionFailed are
// added, along with NewForbiddenError, NewPreconditionFailedError,
// NewModelProviderError and the AppError cause/Unwrap, and IsAppError
// unwraps. See NOTICE.

// Package errors is the application error a handler answers with: a stable
// numeric code, a message fit for the client, and the HTTP status.
package errors

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorCode defines the error code type
type ErrorCode int

// System error codes
const (
	// Common error codes (1000-1999)
	ErrBadRequest         ErrorCode = 1000
	ErrUnauthorized       ErrorCode = 1001
	ErrForbidden          ErrorCode = 1002
	ErrNotFound           ErrorCode = 1003
	ErrMethodNotAllowed   ErrorCode = 1004
	ErrConflict           ErrorCode = 1005
	ErrTooManyRequests    ErrorCode = 1006
	ErrInternalServer     ErrorCode = 1007
	ErrServiceUnavailable ErrorCode = 1008
	ErrTimeout            ErrorCode = 1009
	ErrValidation         ErrorCode = 1010
	// ErrRequestEntityTooLarge is Trelis's: Omnisc answers an oversized
	// upload with ErrBadRequest.
	ErrRequestEntityTooLarge ErrorCode = 1011
	// ErrModelProvider is Trelis's: a model provider rejected a call made
	// while serving a request, because of its credentials, credit, permission
	// or rate limit. The failure is the provider's, not the caller's, so it is
	// a bad gateway.
	ErrModelProvider ErrorCode = 1012
	// ErrPreconditionFailed is Trelis's: the state the caller acted on changed
	// before the request could be applied, so retrying with the new state can
	// succeed.
	ErrPreconditionFailed ErrorCode = 1013
)

// AppError defines the application error structure
type AppError struct {
	Code     ErrorCode `json:"code"`
	Message  string    `json:"message"`
	Details  any       `json:"details,omitempty"`
	HTTPCode int       `json:"-"`

	// cause is the error that led to this one, if any. Unwrap keeps it in the
	// chain so callers can still recognise it, for example the provider's
	// *api.HTTPError behind ErrModelProvider.
	cause error
}

// Error implements the error interface
func (e *AppError) Error() string {
	return fmt.Sprintf("error code: %d, error message: %s", e.Code, e.Message)
}

// Unwrap returns the error behind this one, or nil.
func (e *AppError) Unwrap() error {
	return e.cause
}

// WithDetails adds error details
func (e *AppError) WithDetails(details any) *AppError {
	e.Details = details
	return e
}

// NewBadRequestError creates a bad request error
func NewBadRequestError(message string) *AppError {
	return &AppError{
		Code:     ErrBadRequest,
		Message:  message,
		HTTPCode: http.StatusBadRequest,
	}
}

// NewNotFoundError creates a not found error
func NewNotFoundError(message string) *AppError {
	return &AppError{
		Code:     ErrNotFound,
		Message:  message,
		HTTPCode: http.StatusNotFound,
	}
}

// NewForbiddenError creates a forbidden error: the caller may read the
// resource but may not change it.
func NewForbiddenError(message string) *AppError {
	return &AppError{
		Code:     ErrForbidden,
		Message:  message,
		HTTPCode: http.StatusForbidden,
	}
}

// NewPreconditionFailedError creates a precondition-failed error: the caller
// acted on a state that changed before the request could be applied.
func NewPreconditionFailedError(message string) *AppError {
	return &AppError{
		Code:     ErrPreconditionFailed,
		Message:  message,
		HTTPCode: http.StatusPreconditionFailed,
	}
}

// NewRequestEntityTooLargeError creates a request entity too large error
func NewRequestEntityTooLargeError(message string) *AppError {
	return &AppError{
		Code:     ErrRequestEntityTooLarge,
		Message:  message,
		HTTPCode: http.StatusRequestEntityTooLarge,
	}
}

// NewInternalServerError creates an internal server error
func NewInternalServerError(message string) *AppError {
	if message == "" {
		message = "Internal server error"
	}
	return &AppError{
		Code:     ErrInternalServer,
		Message:  message,
		HTTPCode: http.StatusInternalServerError,
	}
}

// NewModelProviderError creates the bad gateway error a model provider's
// rejection reaches the client as. role names the model role ("embedding",
// "rerank", ...) and status is the provider's HTTP status; cause, the provider
// error itself, stays in the chain. The message never carries the provider's
// body, which can echo the request.
func NewModelProviderError(role string, status int, cause error) *AppError {
	return &AppError{
		Code:     ErrModelProvider,
		Message:  fmt.Sprintf("the %s model provider rejected the call (HTTP %d)", role, status),
		HTTPCode: http.StatusBadGateway,
		cause:    cause,
	}
}

// NewServiceUnavailableError creates a service unavailable error
func NewServiceUnavailableError(message string) *AppError {
	return &AppError{
		Code:     ErrServiceUnavailable,
		Message:  message,
		HTTPCode: http.StatusServiceUnavailable,
	}
}

// NewValidationError creates a validation error
func NewValidationError(message string) *AppError {
	return &AppError{
		Code:     ErrValidation,
		Message:  message,
		HTTPCode: http.StatusBadRequest,
	}
}

// IsAppError checks if the error is an application error. Unlike Omnisc's,
// it also finds an AppError wrapped with %w.
func IsAppError(err error) (*AppError, bool) {
	return errors.AsType[*AppError](err)
}
