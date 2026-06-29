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

// Engine enforces per-day rebalance/fee-set budgets, per-ppm-delta caps,
// per-channel cooldowns, and maximum rebalance fee rates.
type Engine struct {
	mu               sync.Mutex
	cfg              config.Limits
	statePath        string
	cooldown         time.Duration
	dailySpentSat    int64
	dailyFeePpmSpent int64
	lastResetDay     string // "YYYY-MM-DD" in UTC
	channelLastOp    map[uint64]time.Time
}

// ChannelOperationReservation records the previous cooldown state for a
// pre-execution channel operation reservation.
type ChannelOperationReservation struct {
	chanID      uint64
	reserved    time.Time
	previous    time.Time
	hadPrevious bool
}

// FeeSetReservation records previous budget/cooldown state for a pre-write
// fee-set reservation.
type FeeSetReservation struct {
	chanID               uint64
	reserved             time.Time
	previous             time.Time
	hadPrevious          bool
	feeDeltaPpm          int64
	previousDailyFeePpm  int64
	reservedDailyFeePpm  int64
	previousLastResetDay string
	reservedLastResetDay string
}

type channelCooldownReservation struct {
	chanID      uint64
	reserved    time.Time
	previous    time.Time
	hadPrevious bool
}

// RebalanceReservation records previous budget/cooldown state for a
// pre-write rebalance reservation.
type RebalanceReservation struct {
	channels             []channelCooldownReservation
	amountSat            int64
	previousDailySpent   int64
	reservedDailySpent   int64
	previousLastResetDay string
	reservedLastResetDay string
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
	deltaPpm, err := absInt64(deltaPpm)
	if err != nil {
		return fmt.Errorf("fee delta overflows absolute value and exceeds max_fee_ppm_delta %d",
			e.cfg.MaxFeePpmDelta)
	}
	if deltaPpm > e.cfg.MaxFeePpmDelta {
		return fmt.Errorf("fee delta %d ppm exceeds max_fee_ppm_delta %d", deltaPpm, e.cfg.MaxFeePpmDelta)
	}
	return nil
}

// CheckFeeSet returns an error if a fee-set request would violate the daily
// fee ppm budget or per-channel cooldown. It does not reserve the operation.
func (e *Engine) CheckFeeSet(chanID uint64, deltaPpm int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.resetDailyIfNeeded()
	if err := e.checkFeePpmBudgetLocked(deltaPpm); err != nil {
		return err
	}
	if err := e.checkChannelCooldownLocked(chanID); err != nil {
		return err
	}
	if err := e.persistLocked(); err != nil {
		return fmt.Errorf("persist limits state: %w", err)
	}
	return nil
}

// CheckRebalance returns an error if the proposed rebalance violates any limit.
// It does NOT record the operation — call RecordRebalance on success.
func (e *Engine) CheckRebalance(chanID uint64, amtSat, feePpm int64) error {
	return e.CheckRebalanceChannels([]uint64{chanID}, amtSat, feePpm)
}

// CheckRebalanceChannels returns an error if the proposed rebalance violates
// amount, fee, daily budget, or any involved channel cooldown limit.
func (e *Engine) CheckRebalanceChannels(chanIDs []uint64, amtSat,
	feePpm int64) error {

	e.mu.Lock()
	defer e.mu.Unlock()

	e.resetDailyIfNeeded()
	if err := e.checkRebalanceBudgetLocked(amtSat, feePpm); err != nil {
		return err
	}
	for _, chanID := range uniqueChanIDs(chanIDs) {
		if err := e.checkChannelCooldownLocked(chanID); err != nil {
			return err
		}
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

// ReserveFeeSetOperation atomically validates and reserves both the fee-set
// daily ppm budget and the channel cooldown before an external LND write.
func (e *Engine) ReserveFeeSetOperation(chanID uint64,
	deltaPpm int64) (FeeSetReservation, error) {

	e.mu.Lock()
	defer e.mu.Unlock()

	previousLastResetDay := e.lastResetDay
	e.resetDailyIfNeeded()
	if err := e.checkFeePpmBudgetLocked(deltaPpm); err != nil {
		return FeeSetReservation{}, err
	}
	if err := e.checkChannelCooldownLocked(chanID); err != nil {
		return FeeSetReservation{}, err
	}
	feeDeltaPpm, err := absInt64(deltaPpm)
	if err != nil {
		return FeeSetReservation{}, err
	}

	reserved := time.Now()
	reservation := FeeSetReservation{
		chanID:               chanID,
		reserved:             reserved,
		feeDeltaPpm:          feeDeltaPpm,
		previousDailyFeePpm:  e.dailyFeePpmSpent,
		previousLastResetDay: previousLastResetDay,
		reservedLastResetDay: e.lastResetDay,
	}
	if previous, ok := e.channelLastOp[chanID]; ok {
		reservation.previous = previous
		reservation.hadPrevious = true
	}
	e.channelLastOp[chanID] = reserved
	e.dailyFeePpmSpent += feeDeltaPpm
	reservation.reservedDailyFeePpm = e.dailyFeePpmSpent

	if err := e.persistLocked(); err != nil {
		e.restoreFeeSetReservationLocked(reservation)
		return FeeSetReservation{}, fmt.Errorf("persist limits state: %w", err)
	}
	return reservation, nil
}

// ReserveRebalanceOperation atomically validates and reserves the rebalance
// daily sat budget and all involved channel cooldowns before an external LND
// write.
func (e *Engine) ReserveRebalanceOperation(chanIDs []uint64, amtSat,
	feePpm int64) (RebalanceReservation, error) {

	e.mu.Lock()
	defer e.mu.Unlock()

	previousLastResetDay := e.lastResetDay
	e.resetDailyIfNeeded()
	if err := e.checkRebalanceBudgetLocked(amtSat, feePpm); err != nil {
		return RebalanceReservation{}, err
	}
	uniqueChannels := uniqueChanIDs(chanIDs)
	for _, chanID := range uniqueChannels {
		if err := e.checkChannelCooldownLocked(chanID); err != nil {
			return RebalanceReservation{}, err
		}
	}

	reserved := time.Now()
	reservation := RebalanceReservation{
		channels:             make([]channelCooldownReservation, 0, len(uniqueChannels)),
		amountSat:            amtSat,
		previousDailySpent:   e.dailySpentSat,
		previousLastResetDay: previousLastResetDay,
		reservedLastResetDay: e.lastResetDay,
	}
	for _, chanID := range uniqueChannels {
		channel := channelCooldownReservation{chanID: chanID, reserved: reserved}
		if previous, ok := e.channelLastOp[chanID]; ok {
			channel.previous = previous
			channel.hadPrevious = true
		}
		e.channelLastOp[chanID] = reserved
		reservation.channels = append(reservation.channels, channel)
	}
	e.dailySpentSat += amtSat
	reservation.reservedDailySpent = e.dailySpentSat

	if err := e.persistLocked(); err != nil {
		e.restoreRebalanceReservationLocked(reservation)
		return RebalanceReservation{}, fmt.Errorf("persist limits state: %w", err)
	}
	return reservation, nil
}

// RollbackFeeSetOperation restores a fee-set reservation when the external
// write did not happen.
func (e *Engine) RollbackFeeSetOperation(reservation FeeSetReservation) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.restoreFeeSetReservationLocked(reservation)
	if err := e.persistLocked(); err != nil {
		return fmt.Errorf("persist limits state: %w", err)
	}
	return nil
}

// RollbackRebalanceOperation restores a rebalance reservation when the external
// write did not happen.
func (e *Engine) RollbackRebalanceOperation(reservation RebalanceReservation) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.restoreRebalanceReservationLocked(reservation)
	if err := e.persistLocked(); err != nil {
		return fmt.Errorf("persist limits state: %w", err)
	}
	return nil
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

func (e *Engine) restoreFeeSetReservationLocked(reservation FeeSetReservation) {
	ownsReservation := false
	current, ok := e.channelLastOp[reservation.chanID]
	if ok && current.Equal(reservation.reserved) {
		ownsReservation = true
		if reservation.hadPrevious {
			e.channelLastOp[reservation.chanID] = reservation.previous
		} else {
			delete(e.channelLastOp, reservation.chanID)
		}
	}
	if ownsReservation &&
		e.lastResetDay == reservation.reservedLastResetDay &&
		e.dailyFeePpmSpent == reservation.reservedDailyFeePpm {

		e.dailyFeePpmSpent = reservation.previousDailyFeePpm
		e.lastResetDay = reservation.previousLastResetDay
	}
}

func (e *Engine) restoreRebalanceReservationLocked(reservation RebalanceReservation) {
	ownsAllReservations := len(reservation.channels) > 0
	for _, channel := range reservation.channels {
		current, ok := e.channelLastOp[channel.chanID]
		if !ok || !current.Equal(channel.reserved) {
			ownsAllReservations = false
			continue
		}
		if channel.hadPrevious {
			e.channelLastOp[channel.chanID] = channel.previous
		} else {
			delete(e.channelLastOp, channel.chanID)
		}
	}
	if ownsAllReservations &&
		e.lastResetDay == reservation.reservedLastResetDay &&
		e.dailySpentSat == reservation.reservedDailySpent {

		e.dailySpentSat = reservation.previousDailySpent
		e.lastResetDay = reservation.previousLastResetDay
	}
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

func (e *Engine) checkRebalanceBudgetLocked(amtSat, feePpm int64) error {
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
	return nil
}

func (e *Engine) checkFeePpmBudgetLocked(deltaPpm int64) error {
	feeDeltaPpm, err := absInt64(deltaPpm)
	if err != nil {
		return fmt.Errorf("fee delta overflows absolute value")
	}
	if feeDeltaPpm > e.remainingDailyFeePpmBudgetLocked() {
		return fmt.Errorf("fee delta %d ppm would exceed daily_fee_ppm_budget %d (spent %d)",
			feeDeltaPpm, e.cfg.DailyFeePpmBudget, e.dailyFeePpmSpent)
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

// RecordFeeSetOperation refreshes the cooldown timestamp after a successful
// fee-set write. The daily fee budget was already consumed by the reservation.
func (e *Engine) RecordFeeSetOperation(chanID uint64) error {
	return e.RecordChannelOperation(chanID)
}

// RecordRebalanceOperation refreshes the cooldown timestamp after a successful
// rebalance write. The daily sat budget was already consumed by the reservation.
func (e *Engine) RecordRebalanceOperation(chanIDs []uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for _, chanID := range uniqueChanIDs(chanIDs) {
		e.channelLastOp[chanID] = now
	}
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

func (e *Engine) remainingDailyFeePpmBudgetLocked() int64 {
	if e.dailyFeePpmSpent >= e.cfg.DailyFeePpmBudget {
		return 0
	}
	return e.cfg.DailyFeePpmBudget - e.dailyFeePpmSpent
}

func absInt64(value int64) (int64, error) {
	if value == minInt64 {
		return 0, fmt.Errorf("absolute value overflow")
	}
	if value < 0 {
		return -value, nil
	}
	return value, nil
}

func uniqueChanIDs(chanIDs []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(chanIDs))
	out := make([]uint64, 0, len(chanIDs))
	for _, chanID := range chanIDs {
		if _, ok := seen[chanID]; ok {
			continue
		}
		seen[chanID] = struct{}{}
		out = append(out, chanID)
	}
	return out
}

// resetDailyIfNeeded resets the daily counter when the UTC date has changed.
// Must be called with e.mu held.
func (e *Engine) resetDailyIfNeeded() bool {
	today := time.Now().UTC().Format("2006-01-02")
	if e.lastResetDay != today {
		e.dailySpentSat = 0
		e.dailyFeePpmSpent = 0
		e.lastResetDay = today
		return true
	}
	return false
}
