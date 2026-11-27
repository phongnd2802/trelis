package parser

import (
	"context"

	"github.com/phongnd2802/trelis/internal/types"
)

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
