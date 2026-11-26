// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc. Modified for Trelis: only the input and path
// checks the upload path needs; comments translated. See NOTICE.

// Package utils holds small input-safety helpers shared across layers.
package utils

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// xssPatterns match potential XSS payloads.
var xssPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)<script[^>]*>.*?</script>`),
	regexp.MustCompile(`(?i)<iframe[^>]*>.*?</iframe>`),
	regexp.MustCompile(`(?i)<object[^>]*>.*?</object>`),
	regexp.MustCompile(`(?i)<embed[^>]*>.*?</embed>`),
	regexp.MustCompile(`(?i)<embed[^>]*>`),
	regexp.MustCompile(`(?i)<form[^>]*>.*?</form>`),
	regexp.MustCompile(`(?i)<input[^>]*>`),
	regexp.MustCompile(`(?i)<button[^>]*>.*?</button>`),
	regexp.MustCompile(`(?i)javascript:`),
	regexp.MustCompile(`(?i)vbscript:`),
	regexp.MustCompile(`(?i)onload\s*=`),
	regexp.MustCompile(`(?i)onerror\s*=`),
	regexp.MustCompile(`(?i)onclick\s*=`),
	regexp.MustCompile(`(?i)onmouseover\s*=`),
	regexp.MustCompile(`(?i)onfocus\s*=`),
	regexp.MustCompile(`(?i)onblur\s*=`),
}

// ValidateInputSyntax rejects control characters (other than tab, newline
// and carriage return) and invalid UTF-8, and trims surrounding space.
func ValidateInputSyntax(input string) (string, bool) {
	for _, r := range input {
		if r < 32 && r != 9 && r != 10 && r != 13 {
			return "", false
		}
	}
	if !utf8.ValidString(input) {
		return "", false
	}
	return strings.TrimSpace(input), true
}

// ValidateInput is ValidateInputSyntax plus a check for XSS payloads.
func ValidateInput(input string) (string, bool) {
	validatedInput, valid := ValidateInputSyntax(input)
	if !valid {
		return "", false
	}
	for _, pattern := range xssPatterns {
		if pattern.MatchString(input) {
			return "", false
		}
	}
	return validatedInput, true
}

// SafePathUnderBase checks that filePath lies under baseDir, refusing path
// traversal such as ../../, and returns its cleaned absolute path.
func SafePathUnderBase(baseDir, filePath string) (string, error) {
	if baseDir == "" || filePath == "" {
		return "", fmt.Errorf("baseDir and filePath cannot be empty")
	}
	absBase, err := filepath.Abs(filepath.Clean(baseDir))
	if err != nil {
		return "", fmt.Errorf("invalid base dir: %w", err)
	}
	absPath, err := filepath.Abs(filepath.Clean(filePath))
	if err != nil {
		return "", fmt.Errorf("invalid file path: %w", err)
	}
	sep := string(filepath.Separator)
	if absPath != absBase && !strings.HasPrefix(absPath, absBase+sep) {
		return "", fmt.Errorf("path traversal denied: path is outside base directory")
	}
	return absPath, nil
}
