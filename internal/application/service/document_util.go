// Copyright (C) 2026 Tencent. All rights reserved.
// Originally part of WeKnora (https://github.com/Tencent/WeKnora), MIT
// License, by way of Omnisc (internal/application/service/knowledge_util.go).
// Modified for Trelis: SHA-256 instead of MD5, the file name's extension is
// lower-cased, and the video check is folded into the supported set. See
// NOTICE.

package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"strings"

	werrors "github.com/phongnd2802/trelis/internal/errors"
	secutils "github.com/phongnd2802/trelis/internal/utils"
)

// unknownFileType is returned by getFileType when a name carries no extension.
const unknownFileType = "unknown"

// validateImportFileType applies the extension constraints shared by every
// file import path and reports a user-facing reason when one is violated.
func validateImportFileType(fileType string) error {
	fileType = secutils.NormalizeImportExtension(fileType)
	if fileType == "" || fileType == unknownFileType {
		return werrors.NewBadRequestError("Unable to determine file type")
	}
	if !secutils.IsSupportedImportExtension(fileType) {
		return werrors.NewBadRequestError(fmt.Sprintf("Unsupported file type: %s", fileType))
	}
	return nil
}

// getFileType returns the lower-case extension of filename, or "unknown".
func getFileType(filename string) string {
	ext := strings.Split(filename, ".")
	if len(ext) < 2 {
		return unknownFileType
	}
	return secutils.NormalizeImportExtension(ext[len(ext)-1])
}

// validateUploadFile applies the checks every upload path shares: a supported
// extension, a non-empty body, its content hash and a safe file name. It
// returns the file type, the hash and the safe name, or a client-facing error.
func validateUploadFile(file *multipart.FileHeader) (fileType, hash, safeName string, err error) {
	fileName := file.Filename
	fileType = getFileType(fileName)
	if err := validateImportFileType(fileType); err != nil {
		return "", "", "", err
	}
	if file.Size == 0 {
		return "", "", "", werrors.NewBadRequestError("File is empty")
	}
	hash, err = calculateFileHash(file)
	if err != nil {
		return "", "", "", fmt.Errorf("hash upload: %w", err)
	}
	safeName, isValid := secutils.ValidateInput(fileName)
	if !isValid || safeName == "" {
		return "", "", "", werrors.NewValidationError("File name contains illegal characters")
	}
	return fileType, hash, safeName, nil
}

// calculateFileHash returns the hex SHA-256 of an uploaded file.
func calculateFileHash(file *multipart.FileHeader) (string, error) {
	f, err := file.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
