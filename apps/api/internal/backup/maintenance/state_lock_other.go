//go:build !linux && !windows

package maintenance

import "os"

func trustedMaintenanceInfo(os.FileInfo) bool   { return false }
func trustedMaintenanceParent(os.FileInfo) bool { return false }

func lockStateFile(*os.File) error    { return ErrLocked }
func unlockStateFile(*os.File)        {}
func syncStateDirectory(string) error { return ErrState }
