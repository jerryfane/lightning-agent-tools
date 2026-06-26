// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

//go:build windows

package daemon

import "os"

func checkSocketDirOwner(_ string, _ os.FileInfo) error {
	return nil
}
