// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc (internal/middleware/error_handler.go and
// internal/handler/swagger_response.go). Modified for Trelis: written for
// net/http instead of gin, so a handler calls writeError where Omnisc calls
// c.Error and lets middleware render. See NOTICE.

// Package handler turns HTTP requests into service calls and service
// results into Omnisc's response envelope:
//
//	{"success": true, "data": ...}
//	{"success": true, "data": [...], "total": n, "page": p, "page_size": s}
//	{"success": false, "error": {"code": 1003, "message": "...", "details": ...}}
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/phongnd2802/trelis/internal/errors"
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

type errorBody struct {
	Code    errors.ErrorCode `json:"code"`
	Message string           `json:"message"`
	Details any              `json:"details,omitempty"`
}

type errorResponse struct {
	Success bool      `json:"success"`
	Error   errorBody `json:"error"`
}

func writeData(w http.ResponseWriter, status int, data any) {
	writeJSON(w, status, DataResponse{Success: true, Data: data})
}

func writePage(w http.ResponseWriter, page *types.PageResult) {
	writeJSON(w, http.StatusOK, PageResponse{
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
		writeJSON(w, appErr.HTTPCode, errorResponse{Error: errorBody{
			Code:    appErr.Code,
			Message: appErr.Message,
			Details: appErr.Details,
		}})
		return
	}
	slog.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, errorResponse{Error: errorBody{
		Code:    errors.ErrInternalServer,
		Message: "Internal server error",
	}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
