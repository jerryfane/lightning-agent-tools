// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package limits_test

import (
	"testing"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/limits"
)

const maxInt64 = int64(1<<63 - 1)

func newEngine(t *testing.T, cfg config.Limits) *limits.Engine {
	t.Helper()
	eng, err := limits.New(cfg)
	if err != nil {
		t.Fatalf("limits.New: %v", err)
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
