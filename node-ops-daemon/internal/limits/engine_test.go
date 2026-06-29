// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package limits_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/limits"
)

const (
	maxInt64 = int64(1<<63 - 1)
	minInt64 = -maxInt64 - 1
)

func newEngine(t *testing.T, cfg config.Limits) *limits.Engine {
	t.Helper()
	eng, err := limits.New(cfg)
	if err != nil {
		t.Fatalf("limits.New: %v", err)
	}
	return eng
}

func newPersistentEngine(t *testing.T, cfg config.Limits, path string) *limits.Engine {
	t.Helper()
	eng, err := limits.NewPersistent(cfg, path)
	if err != nil {
		t.Fatalf("limits.NewPersistent: %v", err)
	}
	return eng
}

func TestCheckFeeDelta_WithinCap(t *testing.T) {
	eng := newEngine(t, config.Limits{MaxFeePpmDelta: 100, PerChannelCooldown: "0s"})
	if err := eng.CheckFeeDelta(50); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if err := eng.CheckFeeDelta(-50); err != nil {
		t.Errorf("expected nil for negative within cap, got %v", err)
	}
	if err := eng.CheckFeeDelta(100); err != nil {
		t.Errorf("expected nil at exact cap, got %v", err)
	}
}

func TestCheckFeeDelta_ExceedsCap(t *testing.T) {
	eng := newEngine(t, config.Limits{MaxFeePpmDelta: 100, PerChannelCooldown: "0s"})
	if err := eng.CheckFeeDelta(101); err == nil {
		t.Error("expected error for delta exceeding cap")
	}
	if err := eng.CheckFeeDelta(-101); err == nil {
		t.Error("expected error for negative delta exceeding cap")
	}
	if err := eng.CheckFeeDelta(minInt64); err == nil {
		t.Error("expected error for minimum int64 delta")
	}
}

func TestCheckRebalance_FeePpmCap(t *testing.T) {
	eng := newEngine(t, config.Limits{
		DailyRebalanceBudgetSat: 1_000_000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "0s",
		RebalanceMaxFeePpm:      500,
	})
	if err := eng.CheckRebalance(1, 100, 600); err == nil {
		t.Error("expected error: fee_ppm exceeds rebalance_max_fee_ppm")
	}
	if err := eng.CheckRebalance(1, 100, 500); err != nil {
		t.Errorf("expected nil at exact fee cap, got %v", err)
	}
}

func TestCheckRebalance_DailyBudget(t *testing.T) {
	eng := newEngine(t, config.Limits{
		DailyRebalanceBudgetSat: 1000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "0s",
		RebalanceMaxFeePpm:      1000,
	})

	if err := eng.CheckRebalance(1, 500, 100); err != nil {
		t.Fatalf("first rebalance should be allowed: %v", err)
	}
	eng.RecordRebalance(1, 500)

	if err := eng.CheckRebalance(2, 600, 100); err == nil {
		t.Error("expected error: would exceed daily budget")
	}
	if err := eng.CheckRebalance(2, 500, 100); err != nil {
		t.Errorf("exact remaining budget should be allowed: %v", err)
	}
}

func TestCheckRebalance_DailyBudgetOverflow(t *testing.T) {
	eng := newEngine(t, config.Limits{
		DailyRebalanceBudgetSat: 1000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "0s",
		RebalanceMaxFeePpm:      1000,
	})

	eng.RecordRebalance(1, 1)
	if err := eng.CheckRebalance(2, maxInt64, 100); err == nil {
		t.Error("expected near-max amount to exceed remaining budget")
	}

	eng.RecordRebalance(1, maxInt64)
	if err := eng.CheckRebalance(2, 1, 100); err == nil {
		t.Error("expected saturated daily spent counter to reject more budget")
	}
}

func TestCheckRebalance_RejectsNegativeValues(t *testing.T) {
	eng := newEngine(t, config.Limits{
		DailyRebalanceBudgetSat: 1000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "0s",
		RebalanceMaxFeePpm:      1000,
	})

	if err := eng.CheckRebalance(1, -1, 100); err == nil {
		t.Error("expected error for negative amount")
	}
	if err := eng.CheckRebalance(1, 100, -1); err == nil {
		t.Error("expected error for negative fee ppm")
	}
}

func TestCheckRebalance_Cooldown(t *testing.T) {
	eng := newEngine(t, config.Limits{
		DailyRebalanceBudgetSat: 1_000_000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "1h",
		RebalanceMaxFeePpm:      1000,
	})

	if err := eng.CheckRebalance(42, 100, 50); err != nil {
		t.Fatalf("first op should be allowed: %v", err)
	}
	eng.RecordRebalance(42, 100)

	// Same channel within cooldown window.
	if err := eng.CheckRebalance(42, 100, 50); err == nil {
		t.Error("expected cooldown error on second immediate op")
	}

	// Different channel is unaffected.
	if err := eng.CheckRebalance(99, 100, 50); err != nil {
		t.Errorf("different channel should not be in cooldown: %v", err)
	}
}

func TestPersistentDailyBudgetSurvivesRestart(t *testing.T) {
	cfg := config.Limits{
		DailyRebalanceBudgetSat: 1000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "0s",
		RebalanceMaxFeePpm:      1000,
	}
	path := filepath.Join(t.TempDir(), "limits-state.json")

	eng := newPersistentEngine(t, cfg, path)
	if err := eng.CheckRebalance(1, 600, 100); err != nil {
		t.Fatalf("first rebalance should be allowed: %v", err)
	}
	if err := eng.RecordRebalance(1, 600); err != nil {
		t.Fatalf("RecordRebalance: %v", err)
	}

	restarted := newPersistentEngine(t, cfg, path)
	if err := restarted.CheckRebalance(2, 500, 100); err == nil {
		t.Fatal("expected persisted daily budget exhaustion after restart")
	}
	if err := restarted.CheckRebalance(2, 400, 100); err != nil {
		t.Fatalf("expected exact persisted remaining budget to be allowed: %v", err)
	}
}

func TestReserveRebalanceOperationPersistsBudgetAndCooldowns(t *testing.T) {
	cfg := config.Limits{
		DailyRebalanceBudgetSat: 1000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "1h",
		RebalanceMaxFeePpm:      1000,
	}
	path := filepath.Join(t.TempDir(), "limits-state.json")

	eng := newPersistentEngine(t, cfg, path)
	if _, err := eng.ReserveRebalanceOperation([]uint64{1, 2}, 600, 100); err != nil {
		t.Fatalf("ReserveRebalanceOperation: %v", err)
	}
	if err := eng.RecordRebalanceOperation([]uint64{1, 2}); err != nil {
		t.Fatalf("RecordRebalanceOperation: %v", err)
	}

	restarted := newPersistentEngine(t, cfg, path)
	if err := restarted.CheckRebalanceChannels([]uint64{3, 4}, 500, 100); err == nil {
		t.Fatal("expected persisted daily budget exhaustion after restart")
	}
	if err := restarted.CheckRebalanceChannels([]uint64{3, 2}, 100, 100); err == nil ||
		!strings.Contains(err.Error(), "cooldown") {

		t.Fatalf("expected persisted incoming channel cooldown, got %v", err)
	}
}

func TestRollbackRebalanceOperationRestoresBudgetAndCooldowns(t *testing.T) {
	cfg := config.Limits{
		DailyRebalanceBudgetSat: 1000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "1h",
		RebalanceMaxFeePpm:      1000,
	}
	path := filepath.Join(t.TempDir(), "limits-state.json")

	eng := newPersistentEngine(t, cfg, path)
	reservation, err := eng.ReserveRebalanceOperation([]uint64{42, 43}, 1000, 100)
	if err != nil {
		t.Fatalf("ReserveRebalanceOperation: %v", err)
	}
	if err := eng.RollbackRebalanceOperation(reservation); err != nil {
		t.Fatalf("RollbackRebalanceOperation: %v", err)
	}

	restarted := newPersistentEngine(t, cfg, path)
	if _, err := restarted.ReserveRebalanceOperation([]uint64{42, 43}, 1000, 100); err != nil {
		t.Fatalf("rollback should restore budget and cooldowns: %v", err)
	}
}

func TestPersistentFeeSetBudgetSurvivesRestart(t *testing.T) {
	cfg := config.Limits{
		DailyRebalanceBudgetSat: 1000,
		DailyFeePpmBudget:       15,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "0s",
		RebalanceMaxFeePpm:      1000,
	}
	path := filepath.Join(t.TempDir(), "limits-state.json")

	eng := newPersistentEngine(t, cfg, path)
	reservation, err := eng.ReserveFeeSetOperation(1, 10)
	if err != nil {
		t.Fatalf("ReserveFeeSetOperation: %v", err)
	}
	if err := eng.RecordFeeSetOperation(1); err != nil {
		t.Fatalf("RecordFeeSetOperation: %v", err)
	}

	restarted := newPersistentEngine(t, cfg, path)
	if _, err := restarted.ReserveFeeSetOperation(2, 6); err == nil {
		t.Fatal("expected persisted daily fee budget exhaustion after restart")
	}
	if err := restarted.RollbackFeeSetOperation(reservation); err != nil {
		t.Fatalf("stale rollback should be harmless: %v", err)
	}
	if _, err := restarted.ReserveFeeSetOperation(2, 5); err != nil {
		t.Fatalf("expected exact persisted remaining budget to be allowed: %v", err)
	}
}

func TestRollbackFeeSetOperationRestoresBudgetAndCooldown(t *testing.T) {
	cfg := config.Limits{
		DailyRebalanceBudgetSat: 1000,
		DailyFeePpmBudget:       10,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "1h",
		RebalanceMaxFeePpm:      1000,
	}
	path := filepath.Join(t.TempDir(), "limits-state.json")

	eng := newPersistentEngine(t, cfg, path)
	reservation, err := eng.ReserveFeeSetOperation(42, 10)
	if err != nil {
		t.Fatalf("ReserveFeeSetOperation: %v", err)
	}
	if err := eng.RollbackFeeSetOperation(reservation); err != nil {
		t.Fatalf("RollbackFeeSetOperation: %v", err)
	}

	restarted := newPersistentEngine(t, cfg, path)
	if _, err := restarted.ReserveFeeSetOperation(42, 10); err != nil {
		t.Fatalf("rollback should restore budget and cooldown: %v", err)
	}
}

func TestPersistentOldStateDefaultsFeeBudgetSpent(t *testing.T) {
	cfg := config.Limits{
		DailyRebalanceBudgetSat: 1000,
		DailyFeePpmBudget:       10,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "0s",
		RebalanceMaxFeePpm:      1000,
	}
	path := filepath.Join(t.TempDir(), "limits-state.json")
	if err := os.WriteFile(path, []byte(
		`{"daily_spent_sat":0,"last_reset_day":"2026-01-01","channel_last_op":{}}`,
	), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	eng := newPersistentEngine(t, cfg, path)
	if _, err := eng.ReserveFeeSetOperation(1, 10); err != nil {
		t.Fatalf("old state should default daily fee ppm spent to zero: %v", err)
	}
}

func TestPersistentCooldownSurvivesRestart(t *testing.T) {
	cfg := config.Limits{
		DailyRebalanceBudgetSat: 1_000_000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "1h",
		RebalanceMaxFeePpm:      1000,
	}
	path := filepath.Join(t.TempDir(), "limits-state.json")

	eng := newPersistentEngine(t, cfg, path)
	if err := eng.RecordChannelOperation(42); err != nil {
		t.Fatalf("RecordChannelOperation: %v", err)
	}

	restarted := newPersistentEngine(t, cfg, path)
	if err := restarted.CheckChannelCooldown(42); err == nil ||
		!strings.Contains(err.Error(), "cooldown") {

		t.Fatalf("expected persisted cooldown after restart, got %v", err)
	}
	if err := restarted.CheckChannelCooldown(99); err != nil {
		t.Fatalf("different channel should not be in cooldown: %v", err)
	}
}

func TestNewPersistentRejectsCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits-state.json")
	if err := os.WriteFile(path, []byte(`{"daily_spent_sat":-1}`), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := limits.NewPersistent(config.Limits{PerChannelCooldown: "0s"}, path)
	if err == nil || !strings.Contains(err.Error(), "daily_spent_sat") {
		t.Fatalf("expected corrupt state error, got %v", err)
	}
}

func TestNewPersistentRejectsIncompleteState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing daily spent",
			body:    `{"last_reset_day":"2026-01-01","channel_last_op":{}}`,
			wantErr: "daily_spent_sat",
		},
		{
			name:    "missing reset day",
			body:    `{"daily_spent_sat":0,"channel_last_op":{}}`,
			wantErr: "last_reset_day",
		},
		{
			name:    "malformed reset day",
			body:    `{"daily_spent_sat":0,"last_reset_day":"not-a-day","channel_last_op":{}}`,
			wantErr: "last_reset_day",
		},
		{
			name:    "missing channel cooldowns",
			body:    `{"daily_spent_sat":0,"last_reset_day":"2026-01-01"}`,
			wantErr: "channel_last_op",
		},
		{
			name:    "null channel cooldowns",
			body:    `{"daily_spent_sat":0,"last_reset_day":"2026-01-01","channel_last_op":null}`,
			wantErr: "channel_last_op",
		},
		{
			name:    "null channel cooldown entry",
			body:    `{"daily_spent_sat":0,"last_reset_day":"2026-01-01","channel_last_op":{"42":null}}`,
			wantErr: "channel_last_op",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "limits-state.json")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			_, err := limits.NewPersistent(config.Limits{PerChannelCooldown: "0s"}, path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q error, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestRollbackChannelOperationPreservesNewerCooldown(t *testing.T) {
	cfg := config.Limits{
		DailyRebalanceBudgetSat: 1_000_000,
		MaxFeePpmDelta:          1000,
		PerChannelCooldown:      "0s",
		RebalanceMaxFeePpm:      1000,
	}
	path := filepath.Join(t.TempDir(), "limits-state.json")

	eng := newPersistentEngine(t, cfg, path)
	older, err := eng.ReserveChannelOperation(42)
	if err != nil {
		t.Fatalf("first ReserveChannelOperation: %v", err)
	}
	if _, err := eng.ReserveChannelOperation(42); err != nil {
		t.Fatalf("second ReserveChannelOperation: %v", err)
	}
	if err := eng.RollbackChannelOperation(older); err != nil {
		t.Fatalf("RollbackChannelOperation: %v", err)
	}

	cfg.PerChannelCooldown = "1h"
	restarted := newPersistentEngine(t, cfg, path)
	if err := restarted.CheckChannelCooldown(42); err == nil ||
		!strings.Contains(err.Error(), "cooldown") {

		t.Fatalf("expected newer persisted cooldown after stale rollback, got %v", err)
	}
}

func TestNewEngine_BadCooldown(t *testing.T) {
	_, err := limits.New(config.Limits{PerChannelCooldown: "not-a-duration"})
	if err == nil {
		t.Error("expected error for invalid duration")
	}
}

func TestNewEngine_NegativeCooldown(t *testing.T) {
	_, err := limits.New(config.Limits{PerChannelCooldown: "-1h"})
	if err == nil {
		t.Error("expected error for negative duration")
	}
}
