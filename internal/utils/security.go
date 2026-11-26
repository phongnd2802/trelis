// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc. Modified for Trelis: only the input check
// the upload path needs; comments translated. See NOTICE.

// Package utils holds small helpers shared across layers: input checks, and
// API key generation and hashing.
package utils

import (
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
