// Package parser turns uploaded documents into Markdown.
//
// The only engine today is anydoc (internal/parser/anydoc), which converts
// office documents in-process through a statically linked Rust archive. Builds
// without the `anydoc` tag still compile; Parse then returns ErrUnavailable.
package parser

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/phongnd2802/trelis/internal/parser/anydoc"
)

// Re-exported so callers do not need to import the engine package.
var (
	ErrUnavailable   = anydoc.ErrUnavailable
	ErrResourceLimit = anydoc.ErrResourceLimit
	ErrUnsupported   = errors.New("parser: unsupported file type")
)

// Image is one image embedded in the source document.
type Image struct {
	// Path is how the Markdown references the image ("images/image-1.png").
	Path      string
	MediaType string
	Alt       string
	// Section is the nearest heading above the image.
	Section string
	Data    []byte
}

// Result is a parsed document.
type Result struct {
	Markdown string
	Images   []Image
	// ImagesError is set when text was converted but images could not be
	// extracted; the Markdown is still usable.
	ImagesError error
	// NeedsOCR is true for PDFs without a usable text layer (scanned pages).
	NeedsOCR bool
}

// Options tunes one Parse call.
type Options struct {
	// WithImages extracts embedded images and links them in place.
	WithImages bool
}

// Supports reports whether fileName has a type the parser converts.
func Supports(fileName string) bool { return anydoc.Supports("", fileName) }

// SupportedFileTypes lists the extensions (without dot) the parser converts.
func SupportedFileTypes() []string { return anydoc.SupportedFileTypes() }

// Available reports whether the engine is linked into this binary, and why not.
func Available() (bool, string) { return anydoc.Available(), anydoc.UnavailableReason() }

// EngineVersion is the anydoc release linked into this binary ("" if none).
func EngineVersion() string { return anydoc.Version() }

// Parse converts data to Markdown. The format is chosen from fileName's
// extension, so CSV (which has no content signature) is handled too.
func Parse(fileName string, data []byte, opts Options) (*Result, error) {
	format, ok := anydoc.FormatForFile("", fileName)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, strings.TrimPrefix(filepath.Ext(fileName), "."))
	}

	converted, err := anydoc.Convert(data, anydoc.Options{Format: format, WithAssets: opts.WithImages})
	if err != nil {
		if format == "pdf" && anydoc.PDFNeedsOCR(err) {
			return &Result{NeedsOCR: true}, err
		}
		return nil, err
	}

	result := &Result{Markdown: converted.Markdown, ImagesError: converted.AssetsError}
	for _, asset := range converted.Assets {
		result.Images = append(result.Images, Image{
			Path:      anydoc.ImageDir + asset.Name,
			MediaType: asset.MediaType,
			Alt:       asset.Alt,
			Section:   asset.Section,
			Data:      asset.Data,
		})
	}
	return result, nil
}
