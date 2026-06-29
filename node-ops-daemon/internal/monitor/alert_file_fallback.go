// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

//go:build !unix

package monitor

import "os"

func checkAlertDirOwner(_ string, _ os.FileInfo) error {
	return nil
}

func openAlertFileNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
}
