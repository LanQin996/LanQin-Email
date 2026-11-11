package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"lanqin-email-api/internal/backup/maintenance"
)

// Passwords are supplied through a private file, never a command-line value.
// A single terminal newline is a file delimiter; other whitespace is preserved.
func readBackupPasswordFile(name string) ([]byte, error) {
	invalid := errors.New("cannot read private backup password file")
	abs, err := filepath.Abs(name)
	if err != nil || maintenance.ValidatePlanFile(abs) != nil {
		return nil, invalid
	}
	before, err := os.Lstat(abs)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > 1026 {
		return nil, invalid
	}
	file, err := os.Open(abs)
	if err != nil {
		return nil, invalid
	}
	opened, statErr := file.Stat()
	data, readErr := io.ReadAll(io.LimitReader(file, 1027))
	after, afterErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || readErr != nil || afterErr != nil || closeErr != nil || !os.SameFile(before, opened) || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || int64(len(data)) != before.Size() {
		clear(data)
		return nil, invalid
	}
	if bytes.HasSuffix(data, []byte("\r\n")) {
		data = data[:len(data)-2]
	} else if bytes.HasSuffix(data, []byte("\n")) {
		data = data[:len(data)-1]
	}
	if len(data) < 1 || len(data) > 1024 {
		clear(data)
		return nil, invalid
	}
	return data, nil
}

func validNewBackupPassword(password []byte) bool {
	value := string(password)
	characters := utf8.RuneCount(password)
	return utf8.Valid(password) && characters >= 8 && characters <= 128 && strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) < 0
}
