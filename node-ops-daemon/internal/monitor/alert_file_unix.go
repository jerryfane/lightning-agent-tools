// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

//go:build unix

package monitor

import (
	"fmt"
	"os"
	"syscall"
)

func checkAlertDirOwner(dir string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect alert dir owner %s: unsupported stat type", dir)
	}
	uid := uint32(os.Geteuid())
	if stat.Uid != uid {
		return fmt.Errorf("alert dir %s owner uid %d does not match process uid %d",
			dir, stat.Uid, uid)
	}
	return nil
}

func openAlertFileNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path,
		syscall.O_CREAT|syscall.O_WRONLY|syscall.O_APPEND|
			syscall.O_NOFOLLOW|syscall.O_CLOEXEC,
		0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
