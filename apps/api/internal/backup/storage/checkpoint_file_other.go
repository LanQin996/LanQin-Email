//go:build !linux && !windows

package storage

import "os"

func checkpointPrivateDirectory(os.FileInfo) bool      { return false }
func checkpointLock(*os.File) error                    { return errCheckpointFile }
func checkpointReplace(*os.Root, string, string) error { return errCheckpointFile }
