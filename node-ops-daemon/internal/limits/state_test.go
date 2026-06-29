// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package limits

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
)

func TestNewPersistentFailsWhenCurrentStateCannotBeRewritten(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based read-only directory test is Unix-specific")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "limits-state.json")
	today := time.Now().UTC().Format("2006-01-02")
	body := `{"daily_spent_sat":0,"last_reset_day":"` + today + `","channel_last_op":{}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("Chmod read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	_, err := NewPersistent(config.Limits{PerChannelCooldown: "0s"}, path)
	if err == nil || !strings.Contains(err.Error(), "limits state") {
		t.Fatalf("expected limits state rewrite error, got %v", err)
	}
}

func TestCheckRebalanceKeepsFailingWhenResetCannotPersist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based read-only directory test is Unix-specific")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "limits-state.json")
	eng := &Engine{
		cfg: config.Limits{
			DailyRebalanceBudgetSat: 1000,
			PerChannelCooldown:      "0s",
			RebalanceMaxFeePpm:      1000,
		},
		statePath:     path,
		lastResetDay:  "2000-01-01",
		dailySpentSat: 500,
		channelLastOp: make(map[uint64]time.Time),
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("Chmod read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })

	first := eng.CheckRebalance(1, 1, 1)
	if first == nil || !strings.Contains(first.Error(), "persist limits state") {
		t.Fatalf("expected first persist failure, got %v", first)
	}
	second := eng.CheckRebalance(1, 1, 1)
	if second == nil || !strings.Contains(second.Error(), "persist limits state") {
		t.Fatalf("expected second persist failure, got %v", second)
	}
}
