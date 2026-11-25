package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/phongnd2802/trelis/internal/parser"
	"github.com/phongnd2802/trelis/internal/types"
)

func main() {
	filePath := "TN06.docx"

	anydocParser := parser.NewAnydocParser(false)
	fileContent, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", filePath, err)
		os.Exit(1)
	}

	fileName := filepath.Base(filePath)
	fileType := strings.TrimPrefix(filepath.Ext(fileName), ".")
	title := strings.TrimSuffix(fileName, filepath.Ext(fileName))

	ctx := context.Background()
	req := &types.ParseRequest{
		FileContent: fileContent,
		FileName:    fileName,
		FileType:    fileType,
		Title:       title,
	}
	result, err := anydocParser.Parse(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", filePath, err)
		os.Exit(1)
	}

	fmt.Println("======Result=====")
	fmt.Println(result.MarkdownContent)
}
