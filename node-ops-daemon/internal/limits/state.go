// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package limits

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type persistentState struct {
	DailySpentSat    int64                `json:"daily_spent_sat"`
	DailyFeePpmSpent int64                `json:"daily_fee_ppm_spent"`
	LastResetDay     string               `json:"last_reset_day"`
	ChannelLastOp    map[uint64]time.Time `json:"channel_last_op"`
}

type persistentStateFile struct {
	DailySpentSat    *int64                `json:"daily_spent_sat"`
	DailyFeePpmSpent *int64                `json:"daily_fee_ppm_spent"`
	LastResetDay     *string               `json:"last_reset_day"`
	ChannelLastOp    *map[uint64]time.Time `json:"channel_last_op"`
}

func (e *Engine) loadState() error {
	body, err := os.ReadFile(e.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read limits state %s: %w", e.statePath, err)
	}

	var state persistentStateFile
	if err := json.Unmarshal(body, &state); err != nil {
		return fmt.Errorf("decode limits state %s: %w", e.statePath, err)
	}
	if state.DailySpentSat == nil {
		return fmt.Errorf("decode limits state %s: missing daily_spent_sat", e.statePath)
	}
	if *state.DailySpentSat < 0 {
		return fmt.Errorf("decode limits state %s: daily_spent_sat must be non-negative", e.statePath)
	}
	if state.DailyFeePpmSpent != nil && *state.DailyFeePpmSpent < 0 {
		return fmt.Errorf("decode limits state %s: daily_fee_ppm_spent must be non-negative", e.statePath)
	}
	if state.LastResetDay == nil {
		return fmt.Errorf("decode limits state %s: missing last_reset_day", e.statePath)
	}
	if _, err := time.Parse("2006-01-02", *state.LastResetDay); err != nil {
		return fmt.Errorf("decode limits state %s: invalid last_reset_day: %w", e.statePath, err)
	}
	if state.ChannelLastOp == nil {
		return fmt.Errorf("decode limits state %s: missing channel_last_op", e.statePath)
	}
	for chanID, last := range *state.ChannelLastOp {
		if last.IsZero() {
			return fmt.Errorf("decode limits state %s: channel_last_op[%d] must be non-zero",
				e.statePath, chanID)
		}
	}

	e.dailySpentSat = *state.DailySpentSat
	if state.DailyFeePpmSpent != nil {
		e.dailyFeePpmSpent = *state.DailyFeePpmSpent
	}
	e.lastResetDay = *state.LastResetDay
	e.channelLastOp = *state.ChannelLastOp
	return nil
}

// persistLocked writes the current mutable limits state atomically. The caller
// must hold e.mu.
func (e *Engine) persistLocked() error {
	if e.statePath == "" {
		return nil
	}

	state := persistentState{
		DailySpentSat:    e.dailySpentSat,
		DailyFeePpmSpent: e.dailyFeePpmSpent,
		LastResetDay:     e.lastResetDay,
		ChannelLastOp:    persistentChannelLastOp(e.channelLastOp),
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode limits state: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(e.statePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create limits state dir %s: %w", dir, err)
	}
	tmpPath := filepath.Join(dir, "."+filepath.Base(e.statePath)+".tmp")
	if err := os.WriteFile(tmpPath, body, 0600); err != nil {
		return fmt.Errorf("write limits state temp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, e.statePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace limits state %s: %w", e.statePath, err)
	}
	if err := os.Chmod(e.statePath, 0600); err != nil {
		return fmt.Errorf("secure limits state %s: %w", e.statePath, err)
	}
	return nil
}

func persistentChannelLastOp(channelLastOp map[uint64]time.Time) map[uint64]time.Time {
	out := make(map[uint64]time.Time, len(channelLastOp))
	for chanID, last := range channelLastOp {
		out[chanID] = last.UTC()
	}
	return out
}
