package parser

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/phongnd2802/trelis/internal/parser/anydoc"
	"github.com/phongnd2802/trelis/internal/types"
)

// ErrUnsupportedFileType means no parser reads the Source File's type. Upload
// accepts only SupportedFileTypes, so ingest meeting it is a bug.
var ErrUnsupportedFileType = errors.New("unsupported file type")

// SupportedFileTypes returns, sorted, every file type ingest can parse: plain
// text plus what anydoc converts. POST /documents accepts exactly these, so an
// upload is never accepted that ingest cannot read.
func SupportedFileTypes() []string {
	all := slices.Collect(maps.Keys(plainTextFileTypes))
	all = append(all, anydoc.SupportedFileTypes()...)
	slices.Sort(all)
	return all
}

// SupportsFileType reports whether ingest can parse a Source File of type ext
// (whether or not the anydoc binding is linked into this build).
func SupportsFileType(ext string) bool {
	ext = NormalizeFileType(ext)
	return isPlainTextFileType(ext) || anydoc.Supports(ext, "")
}

// FormatRouter sends plain text to PlainTextParser and every other supported
// type to AnydocParser.
type FormatRouter struct {
	plainText *PlainTextParser
	anydoc    *AnydocParser
}

// NewFormatRouter returns the parser the ingest service uses.
func NewFormatRouter(extractImages bool) *FormatRouter {
	return &FormatRouter{
		plainText: NewPlainTextParser(),
		anydoc:    NewAnydocParser(extractImages),
	}
}

// Parse turns req's Source File into Markdown through the parser for its type.
func (p *FormatRouter) Parse(ctx context.Context, req *types.ParseRequest) (*types.ParseResult, error) {
	if isPlainTextFileType(fileTypeOf(req)) {
		return p.plainText.Parse(ctx, req)
	}
	return p.anydoc.Parse(ctx, req)
}
