// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc (internal/types/search.go). Modified for Trelis:
// gin binding tags replaced by Validate. See NOTICE.

package types

import "fmt"

// maxPageSize is the largest page a list request may ask for.
const maxPageSize = 1000

// maxPage keeps Offset within the int32 the queries take.
const maxPage = 1_000_000

// Pagination represents pagination parameters
type Pagination struct {
	// Page
	Page int `json:"page"`
	// Page size
	PageSize int `json:"page_size"`
}

// Validate rejects what Omnisc's binding tags (omitempty,min=1) reject: a
// negative page, or a page size above 1000. Zero means "not given" and takes
// the default, as omitempty does. Trelis also caps page, so the offset fits
// the query.
func (p *Pagination) Validate() error {
	if p.Page < 0 || p.Page > maxPage {
		return fmt.Errorf("page must be between 1 and %d", maxPage)
	}
	if p.PageSize < 0 || p.PageSize > maxPageSize {
		return fmt.Errorf("page_size must be between 1 and %d", maxPageSize)
	}
	return nil
}

// GetPage gets the page number, default is 1
func (p *Pagination) GetPage() int {
	if p.Page < 1 {
		return 1
	}
	return p.Page
}

// GetPageSize gets the page size, default is 20
func (p *Pagination) GetPageSize() int {
	if p.PageSize < 1 {
		return 20
	}
	if p.PageSize > maxPageSize {
		return maxPageSize
	}
	return p.PageSize
}

// Offset gets the offset for database query
func (p *Pagination) Offset() int {
	return (p.GetPage() - 1) * p.GetPageSize()
}

// Limit gets the limit for database query
func (p *Pagination) Limit() int {
	return p.GetPageSize()
}

// PageResult represents the pagination query result
type PageResult struct {
	Total    int64 `json:"total"`     // Total number of records
	Page     int   `json:"page"`      // Current page number
	PageSize int   `json:"page_size"` // Page size
	Data     any   `json:"data"`      // Data
}

// NewPageResult creates a new pagination result
func NewPageResult(total int64, page *Pagination, data any) *PageResult {
	return &PageResult{
		Total:    total,
		Page:     page.GetPage(),
		PageSize: page.GetPageSize(),
		Data:     data,
	}
}
