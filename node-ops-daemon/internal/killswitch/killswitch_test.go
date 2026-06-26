// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package killswitch_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/killswitch"
)

func TestActiveWhenFileExists(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "killswitch-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if !killswitch.Active(f.Name()) {
		t.Error("expected killswitch to be active when file exists")
	}
}

func TestInactiveWhenFileAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "KILL")
	if killswitch.Active(path) {
		t.Error("expected killswitch to be inactive for missing file")
	}
}

func TestInactiveAfterRemoval(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "killswitch-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	os.Remove(f.Name())
	if killswitch.Active(f.Name()) {
		t.Error("expected killswitch to be inactive after file is removed")
	}
}

func TestActiveWhenStatErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod permission behavior differs on Windows")
	}

	dir := filepath.Join(t.TempDir(), "blocked")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	if !killswitch.Active(filepath.Join(dir, "STOP")) {
		t.Error("expected stat errors to fail closed as active")
	}
}
