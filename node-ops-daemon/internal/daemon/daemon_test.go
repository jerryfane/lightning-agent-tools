// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/executor"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/ledger"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/queue"
)

func newTestDaemon(t *testing.T, mutate func(*config.Config)) *Daemon {
	return newTestDaemonWithExecutor(t, mutate, newFakeExecutor(map[uint64]executor.FeePolicy{
		1:  {BaseMsat: 1_000, FeePpm: 100},
		42: {BaseMsat: 1_000, FeePpm: 100},
	}))
}

func newTestDaemonWithExecutor(t *testing.T, mutate func(*config.Config),
	exec executor.NodeExecutor) *Daemon {

	t.Helper()

	dir := t.TempDir()
	storeDir := filepath.Join(dir, "node-ops")
	if err := os.Mkdir(storeDir, 0700); err != nil {
		t.Fatalf("Mkdir store dir: %v", err)
	}
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
			LedgerPath:      filepath.Join(storeDir, "ledger.db"),
			LimitsStatePath: filepath.Join(storeDir, "limits-state.json"),
			KillswitchFile:  filepath.Join(storeDir, "STOP"),
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
	mu         sync.Mutex
	current    map[uint64]executor.FeePolicy
	executed   []executor.FeeSetRequest
	onCurrent  func()
	onExecute  func()
	executeErr error
}

func newFakeExecutor(current map[uint64]executor.FeePolicy) *fakeExecutor {
	return &fakeExecutor{current: current}
}

func (f *fakeExecutor) CurrentFeePolicy(_ context.Context,
	chanID uint64) (executor.FeePolicy, error) {

	f.mu.Lock()
	current := f.current[chanID]
	onCurrent := f.onCurrent
	f.mu.Unlock()

	if onCurrent != nil {
		onCurrent()
	}
	return current, nil
}

func (f *fakeExecutor) ExecuteFeeSet(_ context.Context,
	req executor.FeeSetRequest) error {

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.executeErr != nil {
		return f.executeErr
	}
	if f.onExecute != nil {
		f.onExecute()
	}
	f.executed = append(f.executed, req)
	f.current[req.ChanID] = executor.FeePolicy{
		BaseMsat: req.BaseMsat,
		FeePpm:   req.FeePpm,
	}
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
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"old_ppm":100,"fee_ppm":250}`),
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
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"old_ppm":249,"fee_ppm":250}`),
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
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"old_ppm":100,"fee_ppm":110}`),
	})
	if first.Status != "ok" {
		t.Fatalf("expected first request to execute, got %+v", first)
	}

	second := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"old_ppm":110,"fee_ppm":120}`),
	})
	if second.Status != "error" || !strings.Contains(second.Reason, "cooldown") {
		t.Fatalf("expected cooldown rejection, got %+v", second)
	}
}

func TestDispatchCooldownSurvivesDaemonRestart(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "node-ops")
	if err := os.Mkdir(storeDir, 0700); err != nil {
		t.Fatalf("Mkdir store dir: %v", err)
	}
	cfg := &config.Config{
		Limits: config.Limits{
			DailyRebalanceBudgetSat: 1_000,
			MaxFeePpmDelta:          100,
			PerChannelCooldown:      "1h",
			RebalanceMaxFeePpm:      500,
		},
		Approval: config.Approval{
			AutoExecuteBelowPpmDelta: 25,
			RequireApproval:          false,
		},
		Storage: config.Storage{
			LedgerPath:      filepath.Join(storeDir, "ledger.db"),
			LimitsStatePath: filepath.Join(storeDir, "limits-state.json"),
			KillswitchFile:  filepath.Join(storeDir, "STOP"),
		},
	}
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	})

	firstDaemon, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New first daemon: %v", err)
	}
	first := firstDaemon.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if first.Status != "ok" {
		t.Fatalf("expected first request to execute, got %+v", first)
	}
	if err := firstDaemon.Close(); err != nil {
		t.Fatalf("Close first daemon: %v", err)
	}

	restarted, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New restarted daemon: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	second := restarted.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":120}`),
	})
	if second.Status != "error" || !strings.Contains(second.Reason, "cooldown") {
		t.Fatalf("expected persisted cooldown rejection, got %+v", second)
	}
	if len(fake.executed) != 1 {
		t.Fatalf("cooldown restart check should not execute again: %+v", fake.executed)
	}
}

func TestDispatchPersistsCooldownBeforeExecutorWrite(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "node-ops")
	if err := os.Mkdir(storeDir, 0700); err != nil {
		t.Fatalf("Mkdir store dir: %v", err)
	}
	cfg := &config.Config{
		Limits: config.Limits{
			DailyRebalanceBudgetSat: 1_000,
			MaxFeePpmDelta:          100,
			PerChannelCooldown:      "1h",
			RebalanceMaxFeePpm:      500,
		},
		Approval: config.Approval{
			AutoExecuteBelowPpmDelta: 25,
			RequireApproval:          false,
		},
		Storage: config.Storage{
			LedgerPath:      filepath.Join(storeDir, "ledger.db"),
			LimitsStatePath: filepath.Join(storeDir, "limits-state.json"),
			KillswitchFile:  filepath.Join(storeDir, "STOP"),
		},
	}
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	})
	fake.onExecute = func() {
		body, err := os.ReadFile(cfg.Storage.LimitsStatePath)
		if err != nil {
			t.Fatalf("ReadFile limits state during execute: %v", err)
		}
		if !strings.Contains(string(body), `"1":`) {
			t.Fatalf("limits state did not reserve channel before write: %s", body)
		}
	}

	d, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New daemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if resp.Status != "ok" {
		t.Fatalf("expected execution to succeed, got %+v", resp)
	}
}

func TestDispatchRefreshesCooldownAfterExecutorWrite(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "node-ops")
	if err := os.Mkdir(storeDir, 0700); err != nil {
		t.Fatalf("Mkdir store dir: %v", err)
	}
	cfg := &config.Config{
		Limits: config.Limits{
			DailyRebalanceBudgetSat: 1_000,
			MaxFeePpmDelta:          100,
			PerChannelCooldown:      "200ms",
			RebalanceMaxFeePpm:      500,
		},
		Approval: config.Approval{
			AutoExecuteBelowPpmDelta: 25,
			RequireApproval:          false,
		},
		Storage: config.Storage{
			LedgerPath:      filepath.Join(storeDir, "ledger.db"),
			LimitsStatePath: filepath.Join(storeDir, "limits-state.json"),
			KillswitchFile:  filepath.Join(storeDir, "STOP"),
		},
	}
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	})
	fake.onExecute = func() {
		time.Sleep(250 * time.Millisecond)
	}

	d, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New daemon: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	first := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if first.Status != "ok" {
		t.Fatalf("expected first execution to succeed, got %+v", first)
	}
	second := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":120}`),
	})
	if second.Status != "error" || !strings.Contains(second.Reason, "cooldown") {
		t.Fatalf("expected refreshed cooldown rejection, got %+v", second)
	}
}

func TestDispatchExecutorFailureDoesNotPersistCooldown(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "node-ops")
	if err := os.Mkdir(storeDir, 0700); err != nil {
		t.Fatalf("Mkdir store dir: %v", err)
	}
	cfg := &config.Config{
		Limits: config.Limits{
			DailyRebalanceBudgetSat: 1_000,
			MaxFeePpmDelta:          100,
			PerChannelCooldown:      "1h",
			RebalanceMaxFeePpm:      500,
		},
		Approval: config.Approval{
			AutoExecuteBelowPpmDelta: 25,
			RequireApproval:          false,
		},
		Storage: config.Storage{
			LedgerPath:      filepath.Join(storeDir, "ledger.db"),
			LimitsStatePath: filepath.Join(storeDir, "limits-state.json"),
			KillswitchFile:  filepath.Join(storeDir, "STOP"),
		},
	}
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	})
	fake.executeErr = errors.New("lnd rejected update")

	firstDaemon, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New first daemon: %v", err)
	}
	failed := firstDaemon.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if failed.Status != "error" || !strings.Contains(failed.Reason, "lnd rejected") {
		t.Fatalf("expected executor failure, got %+v", failed)
	}
	if err := firstDaemon.Close(); err != nil {
		t.Fatalf("Close first daemon: %v", err)
	}

	fake.executeErr = nil
	restarted, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New restarted daemon: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	second := restarted.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if second.Status != "ok" {
		t.Fatalf("failed execution should not persist cooldown, got %+v", second)
	}
	if len(fake.executed) != 1 {
		t.Fatalf("expected one successful execution after restart, got %+v", fake.executed)
	}
}

func TestDispatchBaseFeeChangeRequiresApproval(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	})
	d := newTestDaemonWithExecutor(t, func(cfg *config.Config) {
		cfg.Approval.RequireApproval = false
	}, fake)

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":2000,"fee_ppm":100}`),
	})
	if resp.Status != "pending" {
		t.Fatalf("expected base fee change to require approval, got %+v", resp)
	}
	if len(fake.executed) != 0 {
		t.Fatalf("base fee change should not auto-execute: %+v", fake.executed)
	}
}

func TestDispatchRejectsPartialFeeSetParams(t *testing.T) {
	d := newTestDaemon(t, func(cfg *config.Config) {
		cfg.Approval.RequireApproval = false
	})

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "missing fee_ppm") {
		t.Fatalf("expected missing fee_ppm rejection, got %+v", resp)
	}
}

func TestDispatchRechecksKillSwitchBeforeExecution(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 0, FeePpm: 100},
	})
	d := newTestDaemonWithExecutor(t, func(cfg *config.Config) {
		cfg.Approval.RequireApproval = false
	}, fake)

	var once sync.Once
	fake.onCurrent = func() {
		once.Do(func() {
			if err := os.WriteFile(d.cfg.Storage.KillswitchFile, []byte("stop"), 0600); err != nil {
				t.Fatalf("write killswitch: %v", err)
			}
		})
	}

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":0,"fee_ppm":110}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "killswitch") {
		t.Fatalf("expected killswitch rejection, got %+v", resp)
	}
	if len(fake.executed) != 0 {
		t.Fatalf("request executed despite kill-switch: %+v", fake.executed)
	}
}

func TestDispatchRechecksKillSwitchBeforeQueueing(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	})
	d := newTestDaemonWithExecutor(t, nil, fake)

	var once sync.Once
	fake.onCurrent = func() {
		once.Do(func() {
			if err := os.WriteFile(d.cfg.Storage.KillswitchFile, []byte("stop"), 0600); err != nil {
				t.Fatalf("write killswitch: %v", err)
			}
		})
	}

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "killswitch") {
		t.Fatalf("expected killswitch rejection, got %+v", resp)
	}
	if pending := d.queue.ListPending(); len(pending) != 0 {
		t.Fatalf("request queued despite kill-switch: %+v", pending)
	}
}

func TestDispatchRechecksKillSwitchBeforeFallbackQueueing(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	})
	d := newTestDaemonWithExecutor(t, func(cfg *config.Config) {
		cfg.Approval.RequireApproval = false
	}, fake)

	var calls int
	fake.onCurrent = func() {
		calls++
		switch calls {
		case 1:
			fake.mu.Lock()
			fake.current[1] = executor.FeePolicy{BaseMsat: 999, FeePpm: 100}
			fake.mu.Unlock()
		case 2:
			if err := os.WriteFile(d.cfg.Storage.KillswitchFile, []byte("stop"), 0600); err != nil {
				t.Fatalf("write killswitch: %v", err)
			}
		}
	}

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "killswitch") {
		t.Fatalf("expected killswitch rejection, got %+v", resp)
	}
	if pending := d.queue.ListPending(); len(pending) != 0 {
		t.Fatalf("request queued despite kill-switch: %+v", pending)
	}
	if len(fake.executed) != 0 {
		t.Fatalf("request executed despite kill-switch: %+v", fake.executed)
	}
}

func TestRunRefusesActiveSocket(t *testing.T) {
	d := newTestDaemon(t, nil)
	sockPath := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	if err := d.Run(context.Background(), sockPath); err == nil ||
		!strings.Contains(err.Error(), "already active") {

		t.Fatalf("expected active socket error, got %v", err)
	}
}

func TestPrepareSocketDirSecuresExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "node-ops")
	if err := os.Mkdir(dir, 0777); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	if err := prepareSocketDir(dir); err != nil {
		t.Fatalf("prepareSocketDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatalf("socket dir still allows group/other access: %03o",
			info.Mode().Perm())
	}
}

func TestNewRejectsUnsafeExistingLedgerDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	if err := os.Mkdir(dir, 0777); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	cfg := config.Defaults()
	cfg.Storage.LedgerPath = filepath.Join(dir, "ledger.db")
	cfg.Storage.LimitsStatePath = filepath.Join(t.TempDir(), "limits-state.json")
	cfg.Storage.KillswitchFile = filepath.Join(t.TempDir(), "STOP")
	d, err := New(cfg, newFakeExecutor(nil))
	if err == nil {
		d.Close()
		t.Fatal("expected unsafe ledger dir rejection")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0077 == 0 {
		t.Fatalf("ledger dir was chmodded despite rejection: %03o",
			info.Mode().Perm())
	}
}

func TestRemoveStaleSocketRejectsNonSocketPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "regular file",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
		},
		{
			name: "directory",
			setup: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatalf("Mkdir: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.sock")
			tc.setup(t, path)

			err := removeStaleSocket(path)
			if err == nil || !strings.Contains(err.Error(), "non-socket") {
				t.Fatalf("expected non-socket rejection, got %v", err)
			}
			if _, statErr := os.Lstat(path); statErr != nil {
				t.Fatalf("path was removed despite rejection: %v", statErr)
			}
		})
	}
}

func TestRemoveStaleSocketRemovesInactiveSocket(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := removeStaleSocket(sockPath); err != nil {
		t.Fatalf("removeStaleSocket: %v", err)
	}
	if _, err := os.Lstat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale socket removal, stat err=%v", err)
	}
}

func TestDispatchKillSwitchHaltsAndLogs(t *testing.T) {
	d := newTestDaemon(t, nil)
	if err := os.WriteFile(d.cfg.Storage.KillswitchFile, []byte("stop"), 0600); err != nil {
		t.Fatalf("write killswitch: %v", err)
	}

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"old_ppm":100,"fee_ppm":101}`),
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

func TestDispatchQueryAuditLogIsQueryableAndReadOnly(t *testing.T) {
	d := newTestDaemon(t, nil)

	rejected := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":250}`),
	})
	if rejected.Status != "error" {
		t.Fatalf("expected rejected setup request, got %+v", rejected)
	}
	status := d.dispatch(Request{Action: "status"})
	if status.Status != "ok" {
		t.Fatalf("expected status setup request, got %+v", status)
	}

	firstQuery := d.dispatch(Request{
		Action: "query_audit_log",
		Params: mustJSON(t, `{"action":"execute_fee_set","limit":10,"newest_first":false}`),
	})
	if firstQuery.Status != "ok" {
		t.Fatalf("expected query ok, got %+v", firstQuery)
	}
	result, ok := firstQuery.Result.(auditQueryResult)
	if !ok {
		t.Fatalf("unexpected query result type: %#v", firstQuery.Result)
	}
	if result.Count != 1 {
		t.Fatalf("expected one filtered entry, got %+v", result)
	}
	entry := result.Entries[0]
	if entry.Action != "execute_fee_set" || entry.Status != "rejected" {
		t.Fatalf("unexpected audit entry: %+v", entry)
	}
	if string(entry.Params) != `{"chan_id":1,"base_msat":1000,"fee_ppm":250}` {
		t.Fatalf("unexpected raw params: %s", entry.Params)
	}

	secondQuery := d.dispatch(Request{
		Action: "query_audit_log",
		Params: mustJSON(t, `{"limit":10,"newest_first":false}`),
	})
	if secondQuery.Status != "ok" {
		t.Fatalf("expected second query ok, got %+v", secondQuery)
	}
	secondResult, ok := secondQuery.Result.(auditQueryResult)
	if !ok {
		t.Fatalf("unexpected second query result type: %#v", secondQuery.Result)
	}
	if secondResult.Count != 2 {
		t.Fatalf("query should not append audit rows, got %+v", secondResult)
	}
}

func TestDispatchQueryAuditLogValidation(t *testing.T) {
	d := newTestDaemon(t, nil)

	resp := d.dispatch(Request{
		Action: "query_audit_log",
		Params: mustJSON(t, `{"limit":0}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "limit") {
		t.Fatalf("expected limit validation error, got %+v", resp)
	}

	resp = d.dispatch(Request{
		Action: "query_audit_log",
		Params: mustJSON(t, `{"offset":-1}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "offset") {
		t.Fatalf("expected offset validation error, got %+v", resp)
	}
}

func TestAuditEntryFromLedgerTruncatesLargeParams(t *testing.T) {
	entry := auditEntryFromLedger(ledger.Entry{
		ID:        1,
		RequestID: "req-1",
		Action:    "execute_fee_set",
		Params:    `{"payload":"` + strings.Repeat("x", maxAuditParamBytes+1) + `"}`,
		Status:    "rejected",
		CreatedAt: time.Now().UTC(),
	})

	if !entry.ParamsTruncated {
		t.Fatalf("expected params to be truncated: %+v", entry)
	}
	if len(entry.ParamsPreview) != maxAuditParamBytes {
		t.Fatalf("preview length = %d, want %d",
			len(entry.ParamsPreview), maxAuditParamBytes)
	}
	if len(entry.Params) != 0 {
		t.Fatalf("truncated entry should omit full params: %s", entry.Params)
	}
}

func TestDispatchKillSwitchSurvivesDaemonRestart(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "node-ops")
	if err := os.Mkdir(storeDir, 0700); err != nil {
		t.Fatalf("Mkdir store dir: %v", err)
	}
	cfg := &config.Config{
		Limits: config.Limits{
			DailyRebalanceBudgetSat: 1_000,
			MaxFeePpmDelta:          100,
			PerChannelCooldown:      "0s",
			RebalanceMaxFeePpm:      500,
		},
		Approval: config.Approval{
			AutoExecuteBelowPpmDelta: 25,
			RequireApproval:          false,
		},
		Storage: config.Storage{
			LedgerPath:      filepath.Join(storeDir, "ledger.db"),
			LimitsStatePath: filepath.Join(storeDir, "limits-state.json"),
			KillswitchFile:  filepath.Join(storeDir, "STOP"),
		},
	}

	firstDaemon, err := New(cfg, newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	}))
	if err != nil {
		t.Fatalf("New first daemon: %v", err)
	}
	if err := firstDaemon.Close(); err != nil {
		t.Fatalf("Close first daemon: %v", err)
	}
	if err := os.WriteFile(cfg.Storage.KillswitchFile, []byte("stop"), 0600); err != nil {
		t.Fatalf("write killswitch: %v", err)
	}

	restarted, err := New(cfg, newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	}))
	if err != nil {
		t.Fatalf("New restarted daemon: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	resp := restarted.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "killswitch") {
		t.Fatalf("expected persisted killswitch rejection, got %+v", resp)
	}
}

func TestReadOnlyActionsContinueDuringKillSwitch(t *testing.T) {
	d := newTestDaemon(t, nil)
	queued := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":42,"base_msat":1000,"fee_ppm":110}`),
	})
	if queued.Status != "pending" {
		t.Fatalf("expected pending setup response, got %+v", queued)
	}
	if err := os.WriteFile(d.cfg.Storage.KillswitchFile, []byte("stop"), 0600); err != nil {
		t.Fatalf("write killswitch: %v", err)
	}

	status := d.dispatch(Request{Action: "status"})
	if status.Status != "ok" {
		t.Fatalf("expected status during killswitch, got %+v", status)
	}
	result, ok := status.Result.(map[string]string)
	if !ok || result["state"] != "stopped" || result["killswitch"] != "active" {
		t.Fatalf("expected active killswitch status, got %#v", status.Result)
	}

	pending := d.dispatch(Request{Action: "list_pending"})
	if pending.Status != "ok" {
		t.Fatalf("expected list_pending during killswitch, got %+v", pending)
	}
	items, ok := pending.Result.([]queue.Item)
	if !ok || len(items) != 1 {
		t.Fatalf("expected one pending item during killswitch, got %#v", pending.Result)
	}
}

func TestDispatchQueuesPendingAndLogs(t *testing.T) {
	d := newTestDaemon(t, nil)

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":42,"base_msat":1000,"old_ppm":100,"fee_ppm":110}`),
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
