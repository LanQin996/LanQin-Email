//go:build linux

package maintenance

import (
	"os"
	"syscall"
	"testing"
)

type ownerFixtureInfo struct {
	os.FileInfo
	uid  uint32
	mode os.FileMode
}

func (info ownerFixtureInfo) Sys() any          { return &syscall.Stat_t{Uid: info.uid} }
func (info ownerFixtureInfo) Mode() os.FileMode { return info.mode }

func TestMaintenancePrivilegedOwnershipDoesNotTrustAPIOwnedState(t *testing.T) {
	current := uint32(os.Geteuid())
	foreign := current + 12345
	for _, mode := range []os.FileMode{0o600, os.ModeDir | 0o700} {
		if privateInfo(ownerFixtureInfo{uid: foreign, mode: mode}) {
			t.Fatal("foreign API-owned private state accepted")
		}
		if !privateInfo(ownerFixtureInfo{uid: current, mode: mode}) {
			t.Fatal("helper-owned private state refused")
		}
	}
	for _, uid := range []uint32{0, current} {
		if !trustedMaintenanceInfo(ownerFixtureInfo{uid: uid, mode: 0o644}) {
			t.Fatal("trusted immutable configuration owner rejected")
		}
		if trustedMaintenanceInfo(ownerFixtureInfo{uid: uid, mode: 0o664}) || trustedMaintenanceInfo(ownerFixtureInfo{uid: uid, mode: 0o646}) {
			t.Fatal("group/other-writable pinned config accepted")
		}
	}
	if trustedMaintenanceInfo(ownerFixtureInfo{uid: foreign, mode: 0o600}) {
		t.Fatal("private mode alone trusted foreign owner")
	}
	if !trustedMaintenanceParent(ownerFixtureInfo{uid: 0, mode: os.ModeDir | os.ModeSticky | 0o777}) || trustedMaintenanceParent(ownerFixtureInfo{uid: foreign, mode: os.ModeDir | os.ModeSticky | 0o777}) {
		t.Fatal("unsafe mutable parent trust")
	}
}
