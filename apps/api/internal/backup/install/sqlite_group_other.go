//go:build !linux

package install

import "os"

// Only in-process synthetic switch tests run outside Linux; Apply rejects the
// production platform before filesystem work. These do not prove UID semantics.
func sqliteSingleFile(info os.FileInfo) bool                 { return info.Mode().IsRegular() }
func sqliteGroupDirectory(info os.FileInfo, gid int) bool    { return info.IsDir() }
func sqliteGroupFile(info, parent os.FileInfo, gid int) bool { return info.Mode().IsRegular() }
