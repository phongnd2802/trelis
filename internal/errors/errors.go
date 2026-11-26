// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc. Modified for Trelis: the common code table is
// kept whole so codes stay stable, the domain codes (tenants, agents, ...)
// and the constructors Trelis does not call are dropped,
// ErrRequestEntityTooLarge is added, and IsAppError unwraps. See NOTICE.

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
)

// AppError defines the application error structure
type AppError struct {
	Code     ErrorCode `json:"code"`
	Message  string    `json:"message"`
	Details  any       `json:"details,omitempty"`
	HTTPCode int       `json:"-"`
}

// Error implements the error interface
func (e *AppError) Error() string {
	return fmt.Sprintf("error code: %d, error message: %s", e.Code, e.Message)
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
