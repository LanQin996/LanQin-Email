//go:build !linux

package install

import "os"

func switchControlledParent(os.FileInfo) bool { return true } // Private simulated tests only; public switching rejects this platform.

func renameSwitchNoReplace(string, string) error { return ErrSwitchPlatform }
