//go:build !unix

package preservation

import (
	"os"
	"runtime"
)

// On Windows the caller manages inherited workspace ACLs. These functions do
// not claim Unix UID/mode enforcement; component and identity checks still run.
func privateInfo(info os.FileInfo) bool  { return runtime.GOOS == "windows" && info.IsDir() }
func safeParent(info os.FileInfo) bool   { return privateInfo(info) }
func safeAncestor(info os.FileInfo) bool { return privateInfo(info) }

// Windows does not expose Unix directory fsync through os.File.Sync. This does
// not claim the Unix durable-directory publication guarantee on Windows.
func syncRoot(_ *os.Root) error { return nil }
