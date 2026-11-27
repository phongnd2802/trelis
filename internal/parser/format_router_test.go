package parser

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/phongnd2802/trelis/internal/types"
)

func TestFormatRouterReadsPlainTextAsMarkdown(t *testing.T) {
	content := "## Heading\n\nBody."
	cases := []struct {
		name     string
		fileType string
		fileName string
	}{
		{name: "md", fileType: "md", fileName: "notes.md"},
		{name: "markdown", fileType: "markdown", fileName: "notes.markdown"},
		{name: "txt", fileType: "txt", fileName: "notes.txt"},
		{name: "file name extension", fileName: "NOTES.MD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewFormatRouter(false).Parse(context.Background(), &types.ParseRequest{
				FileContent: []byte(content),
				FileName:    tc.fileName,
				FileType:    tc.fileType,
			})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got.MarkdownContent != content {
				t.Errorf("MarkdownContent = %q, want the file content unchanged", got.MarkdownContent)
			}
			if got.Metadata["parser"] != "plain-text" {
				t.Errorf("Metadata[parser] = %q, want plain-text", got.Metadata["parser"])
			}
		})
	}
}

func TestFormatRouterRoutesOtherFileTypesToAnydoc(t *testing.T) {
	_, err := NewFormatRouter(false).Parse(context.Background(), &types.ParseRequest{
		FileContent: []byte("not a zip"),
		FileName:    "broken.docx",
		FileType:    "docx",
	})
	if err == nil || !strings.Contains(err.Error(), "anydoc") {
		t.Fatalf("Parse(docx) error = %v, want an anydoc conversion error", err)
	}
}

func TestPlainTextParserRejectsContentThatCannotBeStored(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
		want    string
	}{
		{name: "invalid UTF-8", content: []byte{0xff, 0xfe}, want: "UTF-8"},
		{name: "NUL byte", content: []byte("before\x00after"), want: "NUL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPlainTextParser().Parse(context.Background(), &types.ParseRequest{
				FileContent: tc.content,
				FileName:    "notes.md",
				FileType:    "md",
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v, want a %s error", err, tc.want)
			}
			if strings.Contains(err.Error(), "notes.md") || strings.Contains(err.Error(), "\x00") {
				t.Errorf("Parse error = %q, want no file name or content", err)
			}
		})
	}
}

// TestEveryUploadableFileTypeHasAParser guards the gap #38 found: POST
// /documents accepted md and txt while ingest had no parser for them, so every
// such Document failed. Each type upload accepts must reach a parser that
// reads it; the content is garbage, so a conversion error is fine, but never
// ErrUnsupportedFileType.
func TestEveryUploadableFileTypeHasAParser(t *testing.T) {
	fileTypes := SupportedFileTypes()
	if len(fileTypes) == 0 {
		t.Fatal("SupportedFileTypes is empty")
	}
	for _, ext := range fileTypes {
		t.Run(ext, func(t *testing.T) {
			if !SupportsFileType(ext) {
				t.Errorf("SupportsFileType(%q) = false for a listed type", ext)
			}
			_, err := NewFormatRouter(false).Parse(context.Background(), &types.ParseRequest{
				FileContent: []byte("not a real document"),
				FileName:    "upload." + ext,
				FileType:    ext,
			})
			if errors.Is(err, ErrUnsupportedFileType) {
				t.Fatalf("Parse(%q) = %v: upload accepts this type but no parser reads it", ext, err)
			}
		})
	}
}

func TestFormatRouterRejectsAnUnsupportedFileType(t *testing.T) {
	if SupportsFileType("exe") {
		t.Fatal(`SupportsFileType("exe") = true`)
	}
	_, err := NewFormatRouter(false).Parse(context.Background(), &types.ParseRequest{
		FileContent: []byte("MZ"), FileName: "tool.exe", FileType: "exe",
	})
	if !errors.Is(err, ErrUnsupportedFileType) {
		t.Fatalf("Parse(exe) error = %v, want ErrUnsupportedFileType", err)
	}
}
