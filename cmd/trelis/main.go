// Command trelis converts documents to Markdown with the in-process anydoc engine.
//
//	trelis version
//	trelis parse [-images DIR] FILE   # Markdown to stdout, images to DIR
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/phongnd2802/trelis/internal/parser"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "parse":
		err = runParse(os.Args[2:])
	case "version":
		runVersion()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "trelis: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "trelis:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  trelis version
  trelis parse [-images DIR] FILE

supported types: `+strings.Join(parser.SupportedFileTypes(), ", "))
}

func runVersion() {
	if ok, reason := parser.Available(); !ok {
		fmt.Println("anydoc: unavailable -", reason)
		return
	}
	fmt.Println("anydoc:", parser.EngineVersion())
}

func runParse(args []string) error {
	fs := flag.NewFlagSet("parse", flag.ExitOnError)
	imageDir := fs.String("images", "", "write embedded images under `DIR` (Markdown links stay relative to it)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("parse needs exactly one FILE")
	}
	path := fs.Arg(0)

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	result, err := parser.Parse(filepath.Base(path), data, parser.Options{WithImages: *imageDir != ""})
	if err != nil {
		if result != nil && result.NeedsOCR {
			return fmt.Errorf("%s has no text layer and needs OCR: %w", path, err)
		}
		return err
	}
	if result.ImagesError != nil {
		fmt.Fprintln(os.Stderr, "trelis: warning:", result.ImagesError)
	}

	for _, img := range result.Images {
		dest := filepath.Join(*imageDir, filepath.FromSlash(img.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, img.Data, 0o644); err != nil {
			return err
		}
	}
	if len(result.Images) > 0 {
		fmt.Fprintf(os.Stderr, "trelis: wrote %d image(s) under %s\n", len(result.Images), *imageDir)
	}

	_, err = os.Stdout.WriteString(result.Markdown)
	return err
}
