// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc. Modified for Trelis: the set is the plain-text
// formats plus whatever anydoc converts, instead of Omnisc's fixed list
// (Trelis has no audio, image or Docling paths yet). See NOTICE.

package utils

import (
	"strings"

	"github.com/phongnd2802/trelis/internal/parser/anydoc"
)

// plainTextImportExtensions are read as text without a parser.
var plainTextImportExtensions = map[string]struct{}{
	"txt": {}, "md": {}, "markdown": {},
}

// NormalizeImportExtension lowercases an extension and strips surrounding
// whitespace and one leading dot, so callers can pass "xlsx", ".XLSX" or a
// raw user-supplied file_type.
func NormalizeImportExtension(ext string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
}

// IsSupportedImportExtension reports whether a bare extension can be
// uploaded: plain text, or a format anydoc converts (whether or not the
// binding is linked into this build, so the answer is the same everywhere).
func IsSupportedImportExtension(ext string) bool {
	ext = NormalizeImportExtension(ext)
	if ext == "" {
		return false
	}
	if _, ok := plainTextImportExtensions[ext]; ok {
		return true
	}
	return anydoc.Supports(ext, "")
}
