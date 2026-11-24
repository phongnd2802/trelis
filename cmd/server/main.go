// Command server is a minimal example of using trelis's parser over HTTP.
//
//	make anydoc-lib
//	CGO_ENABLED=1 go run -tags anydoc ./cmd/server
//
//	curl -F file=@report.docx http://localhost:8080/parse            # JSON
//	curl -F file=@report.docx 'http://localhost:8080/parse?format=md' # raw Markdown
//	curl http://localhost:8080/health
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"path/filepath"

	"github.com/phongnd2802/trelis/internal/parser"
)

// maxUpload caps the request body; anydoc also enforces its own parser budgets.
const maxUpload = 50 << 20 // 50 MB

type parseResponse struct {
	FileName    string      `json:"file_name"`
	Markdown    string      `json:"markdown"`
	Images      []imageInfo `json:"images,omitempty"`
	ImagesError string      `json:"images_error,omitempty"`
}

type imageInfo struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Alt       string `json:"alt,omitempty"`
	Section   string `json:"section,omitempty"`
	Size      int    `json:"size"`
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	if ok, reason := parser.Available(); !ok {
		log.Printf("warning: %s", reason)
	} else {
		log.Printf("anydoc %s linked", parser.EngineVersion())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /parse", handleParse)

	log.Printf("listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	ok, reason := parser.Available()
	writeJSON(w, http.StatusOK, map[string]any{
		"anydoc_available": ok,
		"anydoc_version":   parser.EngineVersion(),
		"reason":           reason,
		"file_types":       parser.SupportedFileTypes(),
	})
}

func handleParse(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected a multipart field named \"file\": "+err.Error())
		return
	}
	defer file.Close()

	fileName := filepath.Base(header.Filename)
	if !parser.Supports(fileName) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported file type: "+fileName)
		return
	}
	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}

	result, err := parser.Parse(fileName, data, parser.Options{WithImages: true})
	if err != nil {
		writeError(w, statusFor(result, err), err.Error())
		return
	}

	if r.URL.Query().Get("format") == "md" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = io.WriteString(w, result.Markdown)
		return
	}

	resp := parseResponse{FileName: fileName, Markdown: result.Markdown}
	if result.ImagesError != nil {
		resp.ImagesError = result.ImagesError.Error()
	}
	for _, img := range result.Images {
		resp.Images = append(resp.Images, imageInfo{
			Path: img.Path, MediaType: img.MediaType, Alt: img.Alt, Section: img.Section, Size: len(img.Data),
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// statusFor maps parser errors onto HTTP status codes.
func statusFor(result *parser.Result, err error) int {
	switch {
	case errors.Is(err, parser.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, parser.ErrUnsupported):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, parser.ErrResourceLimit):
		return http.StatusRequestEntityTooLarge
	case result != nil && result.NeedsOCR:
		return http.StatusUnprocessableEntity // scanned PDF: needs an OCR engine
	default:
		return http.StatusUnprocessableEntity
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
