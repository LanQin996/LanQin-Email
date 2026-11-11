//go:build !unix

package handoff

import (
	"os"
	"runtime"
)

// Windows tests exercise copying with caller-managed inherited ACLs. They do
// not establish Unix ownership, hardlink counts or directory fsync guarantees.
func sourcePrivate(info os.FileInfo, _ int) bool {
	return runtime.GOOS == "windows" && info.Mode()&os.ModeSymlink == 0 && (info.IsDir() || info.Mode().IsRegular())
}
func outputPrivate(info os.FileInfo) bool                  { return sourcePrivate(info, 0) }
func outputParent(info os.FileInfo) bool                   { return runtime.GOOS == "windows" && info.IsDir() }
func trustedAncestor(info os.FileInfo, _ int, _ bool) bool { return outputParent(info) }
func syncDirectory(_ *os.Root) error                       { return nil }
