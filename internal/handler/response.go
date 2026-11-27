// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc (internal/middleware/error_handler.go and
// internal/handler/swagger_response.go). Modified for Trelis: written for
// net/http instead of gin, so a handler calls writeError where Omnisc calls
// c.Error and lets middleware render; a non-AppError 500 says only "Internal
// server error" (Omnisc's handlers often wrap err.Error() into it) and the
// cause goes to internal/logger. See NOTICE.

// Package handler turns HTTP requests into service calls and service
// results into Omnisc's response envelope:
//
//	{"success": true, "data": ...}
//	{"success": true, "data": [...], "total": n, "page": p, "page_size": s}
//	{"success": false, "error": {"code": 1003, "message": "...", "details": ...}}
package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/phongnd2802/trelis/internal/errors"
	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/types"
)

// DataResponse is a successful response with one payload.
type DataResponse struct {
	Success bool `json:"success"`
	Data    any  `json:"data"`
}

// PageResponse is a page of a list with the total match count.
type PageResponse struct {
	Success  bool  `json:"success"`
	Data     any   `json:"data"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

// ErrorBody is the error inside ErrorResponse.
type ErrorBody struct {
	Code    errors.ErrorCode `json:"code"`
	Message string           `json:"message"`
	Details any              `json:"details"`
}

// ErrorResponse is the envelope of a failed request:
// {"success": false, "error": {"code", "message", "details"}}.
type ErrorResponse struct {
	Success bool      `json:"success"`
	Error   ErrorBody `json:"error"`
}

func writeData(ctx context.Context, w http.ResponseWriter, status int, data any) {
	writeJSON(ctx, w, status, DataResponse{Success: true, Data: data})
}

func writePage(ctx context.Context, w http.ResponseWriter, page *types.PageResult) {
	writeJSON(ctx, w, http.StatusOK, PageResponse{
		Success:  true,
		Data:     page.Data,
		Total:    page.Total,
		Page:     page.Page,
		PageSize: page.PageSize,
	})
}

// writeError answers with err's AppError envelope; any other error is logged
// and answered as a generic 500, so internal detail never reaches the client.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	if appErr, ok := errors.IsAppError(err); ok {
		writeJSON(r.Context(), w, appErr.HTTPCode, ErrorResponse{Error: ErrorBody{
			Code:    appErr.Code,
			Message: appErr.Message,
			Details: appErr.Details,
		}})
		return
	}
	logger.Ctx(r.Context()).Error("request failed",
		zap.String("method", r.Method),
		zap.String("route", routePattern(r)),
		zap.Error(err))
	writeJSON(r.Context(), w, http.StatusInternalServerError, ErrorResponse{Error: ErrorBody{
		Code:    errors.ErrInternalServer,
		Message: "Internal server error",
	}})
}

// unmatchedRoute stands in for the pattern of a request no route matched; the
// raw path is never logged.
const unmatchedRoute = "unmatched"

func routePattern(r *http.Request) string {
	rc := chi.RouteContext(r.Context())
	if rc == nil {
		return unmatchedRoute
	}
	route := rc.RoutePattern()
	if route == "" || route == "/*" {
		return unmatchedRoute
	}
	return route
}

// internalErrorJSON is the envelope a body that cannot be encoded is answered
// with; it is ErrorResponse's generic 500, written as a constant because the
// normal encoder is what failed.
const internalErrorJSON = `{"success":false,"error":{"code":1007,"message":"Internal server error","details":null}}`

// writeJSON encodes body before touching the response, so a body that cannot
// be encoded (for example a NaN score) still gets a complete 500 envelope
// instead of a 200 header followed by a discarded encode error.
func writeJSON(ctx context.Context, w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		logger.Ctx(ctx).Error("encode response body", zap.Error(err))
		raw, status = []byte(internalErrorJSON), http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// json.Encoder ended every body with a newline; keep that wire shape.
	_, _ = w.Write(append(raw, '\n'))
}
