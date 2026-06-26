// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package limits implements the rate-limiting and cap-enforcement engine.
// All methods are safe for concurrent use.
package limits

import (
	"fmt"
	"sync"
	"time"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
)

const maxInt64 = int64(1<<63 - 1)

// Engine enforces per-day rebalance budgets, per-ppm-delta caps, per-channel
// cooldowns, and maximum rebalance fee rates.
type Engine struct {
	mu            sync.Mutex
	cfg           config.Limits
	cooldown      time.Duration
	dailySpentSat int64
	lastResetDay  string // "YYYY-MM-DD" in UTC
	channelLastOp map[uint64]time.Time
}

// New creates an Engine from the provided limits configuration.
func New(cfg config.Limits) (*Engine, error) {
	d, err := time.ParseDuration(cfg.PerChannelCooldown)
	if err != nil {
		return nil, fmt.Errorf("per_channel_cooldown %q: %w", cfg.PerChannelCooldown, err)
	}
	if d < 0 {
		return nil, fmt.Errorf("per_channel_cooldown %q must be non-negative", cfg.PerChannelCooldown)
	}
	return &Engine{
		cfg:           cfg,
		cooldown:      d,
		channelLastOp: make(map[uint64]time.Time),
	}, nil
}

// CheckFeeDelta returns an error if |deltaPpm| exceeds MaxFeePpmDelta.
func (e *Engine) CheckFeeDelta(deltaPpm int64) error {
	if deltaPpm < 0 {
		deltaPpm = -deltaPpm
	}
	if deltaPpm > e.cfg.MaxFeePpmDelta {
		return fmt.Errorf("fee delta %d ppm exceeds max_fee_ppm_delta %d", deltaPpm, e.cfg.MaxFeePpmDelta)
	}
	return nil
}

// CheckRebalance returns an error if the proposed rebalance violates any limit.
// It does NOT record the operation — call RecordRebalance on success.
func (e *Engine) CheckRebalance(chanID uint64, amtSat, feePpm int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.resetDailyIfNeeded()

	if amtSat < 0 {
		return fmt.Errorf("rebalance amount must be non-negative")
	}
	if feePpm < 0 {
		return fmt.Errorf("fee_ppm must be non-negative")
	}
	if feePpm > e.cfg.RebalanceMaxFeePpm {
		return fmt.Errorf("fee_ppm %d exceeds rebalance_max_fee_ppm %d", feePpm, e.cfg.RebalanceMaxFeePpm)
	}
	if amtSat > e.remainingDailyBudgetLocked() {
		return fmt.Errorf("rebalance %d sat would exceed daily_rebalance_budget_sat %d (spent %d)",
			amtSat, e.cfg.DailyRebalanceBudgetSat, e.dailySpentSat)
	}
	return e.checkChannelCooldownLocked(chanID)
}

// CheckChannelCooldown returns an error if chanID is still cooling down from a
// previous successful operation.
func (e *Engine) CheckChannelCooldown(chanID uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.checkChannelCooldownLocked(chanID)
}

func (e *Engine) checkChannelCooldownLocked(chanID uint64) error {
	if last, ok := e.channelLastOp[chanID]; ok && e.cooldown > 0 {
		if elapsed := time.Since(last); elapsed < e.cooldown {
			remaining := e.cooldown - elapsed
			return fmt.Errorf("channel %d is in cooldown for another %s", chanID, remaining.Round(time.Second))
		}
	}
	return nil
}

// RecordChannelOperation updates the cooldown clock after a successful
// non-rebalance channel operation.
func (e *Engine) RecordChannelOperation(chanID uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.channelLastOp[chanID] = time.Now()
}

// RecordRebalance updates internal accounting after a successful rebalance.
func (e *Engine) RecordRebalance(chanID uint64, amtSat int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if amtSat < 0 {
		return
	}
	e.resetDailyIfNeeded()
	if amtSat > maxInt64-e.dailySpentSat {
		e.dailySpentSat = maxInt64
	} else {
		e.dailySpentSat += amtSat
	}
	e.channelLastOp[chanID] = time.Now()
}

func (e *Engine) remainingDailyBudgetLocked() int64 {
	if e.dailySpentSat >= e.cfg.DailyRebalanceBudgetSat {
		return 0
	}
	return e.cfg.DailyRebalanceBudgetSat - e.dailySpentSat
}

// resetDailyIfNeeded resets the daily counter when the UTC date has changed.
// Must be called with e.mu held.
func (e *Engine) resetDailyIfNeeded() {
	today := time.Now().UTC().Format("2006-01-02")
	if e.lastResetDay != today {
		e.dailySpentSat = 0
		e.lastResetDay = today
	}
}
