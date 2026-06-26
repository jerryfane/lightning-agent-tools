// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/executor"
)

func newTestDaemon(t *testing.T, mutate func(*config.Config)) *Daemon {
	return newTestDaemonWithExecutor(t, mutate, newFakeExecutor(map[uint64]int64{
		1:  100,
		42: 100,
	}))
}

func newTestDaemonWithExecutor(t *testing.T, mutate func(*config.Config),
	exec executor.NodeExecutor) *Daemon {

	t.Helper()

	dir := t.TempDir()
	cfg := &config.Config{
		Limits: config.Limits{
			DailyRebalanceBudgetSat: 1_000,
			MaxFeePpmDelta:          100,
			PerChannelCooldown:      "0s",
			RebalanceMaxFeePpm:      500,
		},
		Approval: config.Approval{
			AutoExecuteBelowPpmDelta: 25,
			RequireApproval:          true,
		},
		Storage: config.Storage{
			LedgerPath:     filepath.Join(dir, "ledger.db"),
			KillswitchFile: filepath.Join(dir, "STOP"),
		},
	}
	if mutate != nil {
		mutate(cfg)
	}

	d, err := New(cfg, exec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

type fakeExecutor struct {
	mu       sync.Mutex
	current  map[uint64]int64
	executed []executor.FeeSetRequest
}

func newFakeExecutor(current map[uint64]int64) *fakeExecutor {
	return &fakeExecutor{current: current}
}

func (f *fakeExecutor) CurrentFeePpm(_ context.Context, chanID uint64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current[chanID], nil
}

func (f *fakeExecutor) ExecuteFeeSet(_ context.Context,
	req executor.FeeSetRequest) error {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.executed = append(f.executed, req)
	f.current[req.ChanID] = req.FeePpm
	return nil
}

func mustJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

func TestDispatchRejectsOverCapAndLogs(t *testing.T) {
	d := newTestDaemon(t, nil)

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"old_ppm":100,"fee_ppm":250}`),
	})
	if resp.Status != "error" {
		t.Fatalf("expected error response, got %q", resp.Status)
	}

	entries, err := d.ledger.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one ledger entry, got %d", len(entries))
	}
	if entries[0].Status != "rejected" ||
		entries[0].Action != "execute_fee_set" {

		t.Fatalf("unexpected ledger entry: %+v", entries[0])
	}
}

func TestDispatchIgnoresCallerOldPpm(t *testing.T) {
	d := newTestDaemon(t, nil)

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"old_ppm":249,"fee_ppm":250}`),
	})
	if resp.Status != "error" {
		t.Fatalf("expected error response, got %+v", resp)
	}
	if !strings.Contains(resp.Reason, "max_fee_ppm_delta") {
		t.Fatalf("expected trusted current fee rejection, got %q", resp.Reason)
	}
}

func TestDispatchAutoExecuteAppliesCooldown(t *testing.T) {
	d := newTestDaemon(t, func(cfg *config.Config) {
		cfg.Approval.RequireApproval = false
		cfg.Limits.PerChannelCooldown = "1h"
	})

	first := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"old_ppm":100,"fee_ppm":110}`),
	})
	if first.Status != "ok" {
		t.Fatalf("expected first request to execute, got %+v", first)
	}

	second := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"old_ppm":110,"fee_ppm":120}`),
	})
	if second.Status != "error" || !strings.Contains(second.Reason, "cooldown") {
		t.Fatalf("expected cooldown rejection, got %+v", second)
	}
}

func TestDispatchKillSwitchHaltsAndLogs(t *testing.T) {
	d := newTestDaemon(t, nil)
	if err := os.WriteFile(d.cfg.Storage.KillswitchFile, []byte("stop"), 0600); err != nil {
		t.Fatalf("write killswitch: %v", err)
	}

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"old_ppm":100,"fee_ppm":101}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "killswitch") {
		t.Fatalf("expected killswitch error, got %+v", resp)
	}

	entries, err := d.ledger.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != "rejected" ||
		entries[0].Reason != "killswitch active" {

		t.Fatalf("unexpected ledger entries: %+v", entries)
	}
}

func TestDispatchQueuesPendingAndLogs(t *testing.T) {
	d := newTestDaemon(t, nil)

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":42,"old_ppm":100,"fee_ppm":110}`),
	})
	if resp.Status != "pending" {
		t.Fatalf("expected pending response, got %+v", resp)
	}
	if pending := d.queue.ListPending(); len(pending) != 1 {
		t.Fatalf("expected one pending item, got %d", len(pending))
	}

	entries, err := d.ledger.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Status != "pending" {
		t.Fatalf("unexpected ledger entries: %+v", entries)
	}
}

func TestExecutionSocketCannotApprove(t *testing.T) {
	d := newTestDaemon(t, nil)

	resp := d.dispatch(Request{
		Action: "approve",
		Params: mustJSON(t, `{"request_id":"pending-id"}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "unknown action") {
		t.Fatalf("expected unknown action error, got %+v", resp)
	}

	entries, err := d.ledger.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "approve" ||
		entries[0].Status != "rejected" {

		t.Fatalf("unexpected ledger entries: %+v", entries)
	}
}
