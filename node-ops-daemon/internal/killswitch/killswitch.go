// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package killswitch implements a file-presence kill-switch that halts all
// daemon execution when the designated file exists on disk.
package killswitch

import "os"

// Active returns true if the kill-switch file is present, indicating that
// execution should be halted. A missing file (or any stat error) is treated
// as inactive.
func Active(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
