// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc. Modified for Trelis: the set is the plain-text
// formats plus whatever anydoc converts, instead of Omnisc's fixed list
// (Trelis has no audio, image or Docling paths yet). See NOTICE.

package utils

import (
	"github.com/phongnd2802/trelis/internal/parser"
)

// NormalizeImportExtension lowercases an extension and strips surrounding
// whitespace and one leading dot, so callers can pass "xlsx", ".XLSX" or a
// raw user-supplied file_type.
func NormalizeImportExtension(ext string) string {
	return parser.NormalizeFileType(ext)
}

// IsSupportedImportExtension reports whether a bare extension can be
// uploaded: exactly the types ingest can parse (parser.SupportedFileTypes),
// whether or not the anydoc binding is linked into this build, so the answer
// is the same everywhere.
func IsSupportedImportExtension(ext string) bool {
	return parser.SupportsFileType(ext)
}
