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

const (
	maxInt64 = int64(1<<63 - 1)
	minInt64 = -maxInt64 - 1
)

// Engine enforces per-day rebalance budgets, per-ppm-delta caps, per-channel
// cooldowns, and maximum rebalance fee rates.
type Engine struct {
	mu            sync.Mutex
	cfg           config.Limits
	statePath     string
	cooldown      time.Duration
	dailySpentSat int64
	lastResetDay  string // "YYYY-MM-DD" in UTC
	channelLastOp map[uint64]time.Time
}

// ChannelOperationReservation records the previous cooldown state for a
// pre-execution channel operation reservation.
type ChannelOperationReservation struct {
	chanID      uint64
	reserved    time.Time
	previous    time.Time
	hadPrevious bool
}

// New creates an Engine from the provided limits configuration.
func New(cfg config.Limits) (*Engine, error) {
	return newEngine(cfg, "")
}

// NewPersistent creates an Engine that persists mutable limit state to
// statePath. A corrupt or unwritable state file fails closed by returning an
// error.
func NewPersistent(cfg config.Limits, statePath string) (*Engine, error) {
	if statePath == "" {
		return nil, fmt.Errorf("limits state path must not be empty")
	}
	return newEngine(cfg, statePath)
}

func newEngine(cfg config.Limits, statePath string) (*Engine, error) {
	d, err := time.ParseDuration(cfg.PerChannelCooldown)
	if err != nil {
		return nil, fmt.Errorf("per_channel_cooldown %q: %w", cfg.PerChannelCooldown, err)
	}
	if d < 0 {
		return nil, fmt.Errorf("per_channel_cooldown %q must be non-negative", cfg.PerChannelCooldown)
	}
	eng := &Engine{
		cfg:           cfg,
		statePath:     statePath,
		cooldown:      d,
		channelLastOp: make(map[uint64]time.Time),
	}
	if statePath != "" {
		if err := eng.loadState(); err != nil {
			return nil, err
		}
		eng.resetDailyIfNeeded()
		if err := eng.persistLocked(); err != nil {
			return nil, err
		}
	}
	return eng, nil
}

// CheckFeeDelta returns an error if |deltaPpm| exceeds MaxFeePpmDelta.
func (e *Engine) CheckFeeDelta(deltaPpm int64) error {
	if deltaPpm == minInt64 {
		return fmt.Errorf("fee delta overflows absolute value and exceeds max_fee_ppm_delta %d",
			e.cfg.MaxFeePpmDelta)
	}
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
	if err := e.checkChannelCooldownLocked(chanID); err != nil {
		return err
	}
	if err := e.persistLocked(); err != nil {
		return fmt.Errorf("persist limits state: %w", err)
	}
	return nil
}

// CheckChannelCooldown returns an error if chanID is still cooling down from a
// previous successful operation.
func (e *Engine) CheckChannelCooldown(chanID uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.checkChannelCooldownLocked(chanID); err != nil {
		return err
	}
	if err := e.persistLocked(); err != nil {
		return fmt.Errorf("persist limits state: %w", err)
	}
	return nil
}

// ReserveChannelOperation atomically validates cooldown, records a new channel
// operation timestamp, and persists it before the caller applies an external
// write. RollbackChannelOperation can undo the reservation if the write does
// not happen.
func (e *Engine) ReserveChannelOperation(chanID uint64) (ChannelOperationReservation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if err := e.checkChannelCooldownLocked(chanID); err != nil {
		return ChannelOperationReservation{}, err
	}
	reserved := time.Now()
	reservation := ChannelOperationReservation{chanID: chanID, reserved: reserved}
	if previous, ok := e.channelLastOp[chanID]; ok {
		reservation.previous = previous
		reservation.hadPrevious = true
	}
	e.channelLastOp[chanID] = reserved
	if err := e.persistLocked(); err != nil {
		if reservation.hadPrevious {
			e.channelLastOp[chanID] = reservation.previous
		} else {
			delete(e.channelLastOp, chanID)
		}
		return ChannelOperationReservation{},
			fmt.Errorf("persist limits state: %w", err)
	}
	return reservation, nil
}

// RollbackChannelOperation restores the state captured by a previous
// reservation. It is used only when the external write did not happen.
func (e *Engine) RollbackChannelOperation(reservation ChannelOperationReservation) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	current, ok := e.channelLastOp[reservation.chanID]
	if !ok || !current.Equal(reservation.reserved) {
		return nil
	}
	if reservation.hadPrevious {
		e.channelLastOp[reservation.chanID] = reservation.previous
	} else {
		delete(e.channelLastOp, reservation.chanID)
	}
	if err := e.persistLocked(); err != nil {
		return fmt.Errorf("persist limits state: %w", err)
	}
	return nil
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
func (e *Engine) RecordChannelOperation(chanID uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.channelLastOp[chanID] = time.Now()
	return e.persistLocked()
}

// RecordRebalance updates internal accounting after a successful rebalance.
func (e *Engine) RecordRebalance(chanID uint64, amtSat int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if amtSat < 0 {
		return nil
	}
	e.resetDailyIfNeeded()
	if amtSat > maxInt64-e.dailySpentSat {
		e.dailySpentSat = maxInt64
	} else {
		e.dailySpentSat += amtSat
	}
	e.channelLastOp[chanID] = time.Now()
	return e.persistLocked()
}

func (e *Engine) remainingDailyBudgetLocked() int64 {
	if e.dailySpentSat >= e.cfg.DailyRebalanceBudgetSat {
		return 0
	}
	return e.cfg.DailyRebalanceBudgetSat - e.dailySpentSat
}

// resetDailyIfNeeded resets the daily counter when the UTC date has changed.
// Must be called with e.mu held.
func (e *Engine) resetDailyIfNeeded() bool {
	today := time.Now().UTC().Format("2006-01-02")
	if e.lastResetDay != today {
		e.dailySpentSat = 0
		e.lastResetDay = today
		return true
	}
	return false
}
