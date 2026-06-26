// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

//go:build windows

package daemon

import "os"

func checkPrivateDirOwner(_ string, _ string, _ os.FileInfo) error {
	return nil
}
