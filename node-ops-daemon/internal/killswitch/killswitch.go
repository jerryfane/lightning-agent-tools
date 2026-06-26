// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package killswitch implements a file-presence kill-switch that halts all
// daemon execution when the designated file exists on disk.
package killswitch

import "os"

// Active returns true if the kill-switch file is present, indicating that
// execution should be halted. Only a clean missing-file result is inactive;
// any other stat error fails closed.
func Active(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !os.IsNotExist(err)
}
