//go:build !linux

package maintenance

import "os"

func productionEnvironmentOps() environmentOps {
	return environmentOps{rename: func(string, string) error { return ErrUnsupported }, syncDirectory: func(string) error { return ErrUnsupported }}
}

// Tests may exercise actual temporary file moves on Windows with explicit
// injected operations. These checks do not claim Linux ownership/fsync QA.
func environmentFilePrivate(info os.FileInfo) bool   { return info.Mode().IsRegular() || info.IsDir() }
func environmentParentPrivate(info os.FileInfo) bool { return info.IsDir() }
