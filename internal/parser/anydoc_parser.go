package parser

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/phongnd2802/trelis/internal/parser/anydoc"
	"github.com/phongnd2802/trelis/internal/types"
)

type AnydocParser struct {
	extractImages bool
}

func (r *AnydocParser) Parse(ctx context.Context, req *types.ParseRequest) (*types.ParseResult, error) {
	format, ok := anydoc.FormatForFile(req.FileType, req.FileName)
	if !ok {
		return nil, fmt.Errorf("anydoc engine does not support file type %q", fileTypeOf(req))
	}

	converted, err := anydoc.Convert(req.FileContent, anydoc.Options{
		Format:     format,
		WithAssets: r.extractImages,
	})

	if format == "pdf" && anydoc.PDFInvalidStructure(err) {
		//fall back another parser
		// TODO...
		return nil, fmt.Errorf("fallback another parser")
	}

	if format == "pdf" && (anydoc.PDFNeedsOCR(err) || pdfMarkdownIsEmpty(converted, err)) {
		//fall back to Docling OCR
		// TODO..
		return nil, fmt.Errorf("fallback to Docling OCR")
	}

	if err != nil {
		return nil, fmt.Errorf("anydoc conversion failed for %q: %w", req.FileName, err)
	}

	if converted.AssetsError != nil {
		slog.WarnContext(ctx, "anydoc parsed text but dropped images", "file", req.FileName, "err", converted.AssetsError)
	}
	return &types.ParseResult{
		MarkdownContent: converted.Markdown,
		ImageRefs:       imageRefsFromAssets(converted.Assets),
		Metadata: map[string]string{
			"parser":         "anydoc-go",
			"anydoc_version": anydoc.Version(),
			"source_format":  format,
		},
	}, nil
}

func imageRefsFromAssets(assets []anydoc.Asset) []types.ImageRef {
	if len(assets) == 0 {
		return nil
	}

	refs := make([]types.ImageRef, 0, len(assets))
	for _, asset := range assets {
		ref := anydoc.ImageDir + asset.Name
		refs = append(refs, types.ImageRef{
			Filename:    asset.Name,
			OriginalRef: ref,
			MimeType:    asset.MediaType,
			ImageData:   asset.Data,
		})
	}

	return refs
}

func pdfMarkdownIsEmpty(converted *anydoc.Result, err error) bool {
	if err != nil || converted == nil {
		return false
	}

	return strings.TrimSpace(converted.Markdown) == ""
}

func fileTypeOf(req *types.ParseRequest) string {
	if ft := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(req.FileType)), "."); ft != "" {
		return ft
	}

	return strings.TrimPrefix(strings.ToLower(filepath.Ext(req.FileName)), ".")
}

func NewAnydocParser(extractImages bool) *AnydocParser {
	return &AnydocParser{
		extractImages: extractImages,
	}
}
