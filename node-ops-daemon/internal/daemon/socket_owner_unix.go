// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

func checkPrivateDirOwner(kind, dir string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect %s dir owner %s: unsupported stat type", kind, dir)
	}
	uid := uint32(os.Geteuid())
	if stat.Uid != uid {
		return fmt.Errorf("%s dir %s owner uid %d does not match process uid %d",
			kind, dir, stat.Uid, uid)
	}
	return nil
}
