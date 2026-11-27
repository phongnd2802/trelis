package parser

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/phongnd2802/trelis/internal/types"
)

// plainTextFileTypes are read as Markdown without a converter. anydoc does
// not convert them, but POST /documents accepts them as Source Files.
var plainTextFileTypes = map[string]struct{}{
	"txt": {}, "md": {}, "markdown": {},
}

// NormalizeFileType lowercases an extension and strips surrounding whitespace
// and one leading dot, so callers can pass "xlsx", ".XLSX" or a raw file_type.
func NormalizeFileType(ext string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
}

// IsPlainTextFileType reports whether ext names a plain-text Source File.
func IsPlainTextFileType(ext string) bool {
	return isPlainTextFileType(NormalizeFileType(ext))
}

// isPlainTextFileType is IsPlainTextFileType for an already-normalized ext.
func isPlainTextFileType(ext string) bool {
	_, ok := plainTextFileTypes[ext]
	return ok
}

// PlainTextParser reads a plain-text Source File as Markdown unchanged.
type PlainTextParser struct{}

// NewPlainTextParser returns a parser for txt, md and markdown Source Files.
func NewPlainTextParser() *PlainTextParser {
	return &PlainTextParser{}
}

// Parse returns the Source File's bytes as Markdown. It rejects content a
// PostgreSQL text column cannot hold, as a parse failure rather than an
// insert error.
func (p *PlainTextParser) Parse(_ context.Context, req *types.ParseRequest) (*types.ParseResult, error) {
	if !utf8.Valid(req.FileContent) {
		return nil, errors.New("the Source File is not valid UTF-8")
	}
	if bytes.IndexByte(req.FileContent, 0) >= 0 {
		return nil, errors.New("the Source File contains a NUL byte")
	}
	return &types.ParseResult{
		MarkdownContent: string(req.FileContent),
		Metadata:        map[string]string{"parser": "plain-text"},
	}, nil
}
