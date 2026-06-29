// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
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
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/monitor"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/queue"
)

const testOperatorToken = "test-operator-token"

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
			DailyFeePpmBudget:       500,
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
		Operator: config.Operator{
			ApprovalSocket:    filepath.Join(storeDir, "operator.sock"),
			ApprovalTokenFile: filepath.Join(storeDir, "operator.token"),
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	installTestOperatorToken(t, cfg, storeDir)

	d, err := New(cfg, exec)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func installTestOperatorToken(t *testing.T, cfg *config.Config, dir string) {
	t.Helper()
	if cfg.Operator.ApprovalSocket == "" {
		cfg.Operator.ApprovalSocket = filepath.Join(dir, "operator.sock")
	}
	if cfg.Operator.ApprovalTokenFile == "" {
		cfg.Operator.ApprovalTokenFile = filepath.Join(dir, "operator.token")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Operator.ApprovalTokenFile), 0700); err != nil {
		t.Fatalf("Mkdir operator token dir: %v", err)
	}
	if err := os.WriteFile(cfg.Operator.ApprovalTokenFile,
		[]byte(testOperatorToken+"\n"), 0600); err != nil {

		t.Fatalf("WriteFile operator token: %v", err)
	}
}

type fakeExecutor struct {
	mu           sync.Mutex
	current      map[uint64]executor.FeePolicy
	health       executor.NodeHealthSnapshot
	healthErr    error
	executed     []executor.FeeSetRequest
	rebalanced   []executor.RebalanceRequest
	onCurrent    func()
	onExecute    func()
	onRebalance  func()
	executeErr   error
	rebalanceErr error
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

func (f *fakeExecutor) ExecuteRebalance(_ context.Context,
	req executor.RebalanceRequest) (executor.RebalanceResult, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rebalanceErr != nil {
		return executor.RebalanceResult{}, f.rebalanceErr
	}
	if f.onRebalance != nil {
		f.onRebalance()
	}
	f.rebalanced = append(f.rebalanced, req)
	return executor.RebalanceResult{
		PaymentHash: "rebalance-hash",
		AmountSat:   req.AmountSat,
		FeeSat:      2,
		FeePpm:      100,
		Status:      "succeeded",
	}, nil
}

func (f *fakeExecutor) NodeHealth(_ context.Context) (executor.NodeHealthSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.healthErr != nil {
		return executor.NodeHealthSnapshot{}, f.healthErr
	}
	return f.health, nil
}

func mustJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

func approvePendingFeeSet(t *testing.T, d *Daemon, pending Response) Response {
	t.Helper()
	if pending.Status != "pending" || pending.RequestID == "" {
		t.Fatalf("expected pending fee-set response, got %+v", pending)
	}
	return d.dispatchOperator(operatorRequest(
		"approve_fee_set",
		mustJSON(t, `{"request_id":"`+pending.RequestID+`"}`),
	))
}

func approvePendingRebalance(t *testing.T, d *Daemon, pending Response) Response {
	t.Helper()
	if pending.Status != "pending" || pending.RequestID == "" {
		t.Fatalf("expected pending rebalance response, got %+v", pending)
	}
	return d.dispatchOperator(operatorRequest(
		"approve_rebalance",
		mustJSON(t, `{"request_id":"`+pending.RequestID+`"}`),
	))
}

func operatorRequest(action string, params json.RawMessage) Request {
	return Request{
		Action:        action,
		Params:        params,
		OperatorToken: testOperatorToken,
	}
}

func socketRoundTrip(t *testing.T, path string, req Request) Response {
	t.Helper()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial %s: %v", path, err)
	}
	defer conn.Close()

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal request: %v", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		t.Fatalf("Write length: %v", err)
	}
	if _, err := conn.Write(body); err != nil {
		t.Fatalf("Write body: %v", err)
	}
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		t.Fatalf("Read response length: %v", err)
	}
	respBody := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(conn, respBody); err != nil {
		t.Fatalf("Read response body: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("Unmarshal response: %v", err)
	}
	return resp
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for socket %s", path)
}

func forceCloseHealthSnapshot() executor.NodeHealthSnapshot {
	return executor.NodeHealthSnapshot{
		OverallStatus: "critical",
		AlertCount:    1,
		CriticalCount: 1,
		NodeID:        "node-123",
		Alias:         "regtest-node",
		SyncedToChain: true,
		SyncedToGraph: true,
		BlockHeight:   144,
		Alerts: []executor.HealthAlert{{
			ID:       "channel:force-close:txid:0",
			Severity: "critical",
			Category: "channel",
			Message:  "Force-closing channel detected",
			Details: map[string]interface{}{
				"channel_point":       "txid:0",
				"remote_node_pub":     "remote",
				"closing_txid":        "close-tx",
				"blocks_til_maturity": 3,
			},
		}},
	}
}

func TestRunMonitorPushesAlertToConfiguredFile(t *testing.T) {
	alertPath := filepath.Join(t.TempDir(), "node-ops", "alerts.jsonl")
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{})
	fake.health = forceCloseHealthSnapshot()
	d := newTestDaemonWithExecutor(t, func(cfg *config.Config) {
		cfg.Monitor.Enabled = true
		cfg.Monitor.PollInterval = "10ms"
		cfg.Monitor.AlertCooldown = "1h"
		cfg.Monitor.AlertChannel = "file"
		cfg.Monitor.AlertPath = alertPath
	}, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	sockPath := filepath.Join(t.TempDir(), "daemon.sock")
	go func() {
		errCh <- d.Run(ctx, sockPath)
	}()

	var body []byte
	deadline := time.After(2 * time.Second)
	for {
		var err error
		body, err = os.ReadFile(alertPath)
		if err == nil && strings.Contains(string(body), "Force-closing channel detected") {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("Run exited before alert was written: %v", err)
		case <-deadline:
			t.Fatalf("timed out waiting for alert file, body=%q err=%v", body, err)
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run: %v", err)
	}

	var event struct {
		ID       string `json:"id"`
		Severity string `json:"severity"`
		Category string `json:"category"`
		Message  string `json:"message"`
		NodeID   string `json:"node_id"`
	}
	line := strings.Split(strings.TrimSpace(string(body)), "\n")[0]
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatalf("decode alert event: %v", err)
	}
	if event.ID != "channel:force-close:txid:0" ||
		event.Severity != "critical" || event.Category != "channel" ||
		event.NodeID != "node-123" {

		t.Fatalf("unexpected alert event: %+v", event)
	}
}

func TestNewRejectsEnabledMonitorWithStubExecutor(t *testing.T) {
	cfg := config.Defaults()
	dir := filepath.Join(t.TempDir(), "node-ops")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatalf("Mkdir store dir: %v", err)
	}
	cfg.Storage.LedgerPath = filepath.Join(dir, "ledger.db")
	cfg.Storage.LimitsStatePath = filepath.Join(dir, "limits-state.json")
	cfg.Storage.KillswitchFile = filepath.Join(dir, "STOP")
	cfg.Monitor.Enabled = true
	cfg.Monitor.AlertPath = filepath.Join(dir, "alerts.jsonl")

	d, err := New(cfg, &executor.StubExecutor{})
	if err == nil {
		d.Close()
		t.Fatal("expected monitor startup rejection with stub executor")
	}
	if !strings.Contains(err.Error(), "concrete node_health reader") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type failingAlertPublisher struct{}

func (f failingAlertPublisher) Publish(context.Context, monitor.AlertEvent) error {
	return errors.New("alert sink down")
}

func TestStatusReportsMonitorLastError(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{})
	fake.health = forceCloseHealthSnapshot()
	mon, err := monitor.New(fake, failingAlertPublisher{}, monitor.Config{
		PollInterval:  10 * time.Millisecond,
		AlertCooldown: time.Minute,
	})
	if err != nil {
		t.Fatalf("monitor New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mon.Run(ctx)
	}()

	deadline := time.After(time.Second)
	for {
		if msg, _, ok := mon.LastError(); ok &&
			strings.Contains(msg, "alert sink down") {

			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for monitor error")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	cfg := config.Defaults()
	cfg.Storage.KillswitchFile = filepath.Join(t.TempDir(), "STOP")
	status := (&Daemon{cfg: cfg, monitor: mon}).statusResult()
	if !strings.Contains(status["monitor_error"], "alert sink down") {
		t.Fatalf("monitor_error = %q", status["monitor_error"])
	}
	if status["monitor_error_at"] == "" {
		t.Fatalf("monitor_error_at was not set: %+v", status)
	}
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

func TestDispatchApprovalExecutionAppliesCooldown(t *testing.T) {
	d := newTestDaemon(t, func(cfg *config.Config) {
		cfg.Approval.RequireApproval = false
		cfg.Limits.PerChannelCooldown = "1h"
	})

	first := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"old_ppm":100,"fee_ppm":110}`),
	})
	if first.Status != "pending" {
		t.Fatalf("expected first request to require approval, got %+v", first)
	}
	approved := approvePendingFeeSet(t, d, first)
	if approved.Status != "ok" {
		t.Fatalf("expected approved request to execute, got %+v", approved)
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
			DailyFeePpmBudget:       500,
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
	installTestOperatorToken(t, cfg, storeDir)

	firstDaemon, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New first daemon: %v", err)
	}
	first := firstDaemon.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if first.Status != "pending" {
		t.Fatalf("expected first request to require approval, got %+v", first)
	}
	approved := approvePendingFeeSet(t, firstDaemon, first)
	if approved.Status != "ok" {
		t.Fatalf("expected approved request to execute, got %+v", approved)
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
			DailyFeePpmBudget:       500,
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
	installTestOperatorToken(t, cfg, storeDir)
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
	if resp.Status != "pending" {
		t.Fatalf("expected request to require approval, got %+v", resp)
	}
	approved := approvePendingFeeSet(t, d, resp)
	if approved.Status != "ok" {
		t.Fatalf("expected approved execution to succeed, got %+v", approved)
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
			DailyFeePpmBudget:       500,
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
	installTestOperatorToken(t, cfg, storeDir)
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
	if first.Status != "pending" {
		t.Fatalf("expected first request to require approval, got %+v", first)
	}
	approved := approvePendingFeeSet(t, d, first)
	if approved.Status != "ok" {
		t.Fatalf("expected approved execution to succeed, got %+v", approved)
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
			DailyFeePpmBudget:       500,
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
	installTestOperatorToken(t, cfg, storeDir)
	fake.executeErr = errors.New("lnd rejected update")

	firstDaemon, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New first daemon: %v", err)
	}
	failed := firstDaemon.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if failed.Status != "pending" {
		t.Fatalf("expected request to require approval, got %+v", failed)
	}
	failedApproval := approvePendingFeeSet(t, firstDaemon, failed)
	if failedApproval.Status != "error" ||
		!strings.Contains(failedApproval.Reason, "lnd rejected") {

		t.Fatalf("expected executor failure, got %+v", failedApproval)
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
	if second.Status != "pending" {
		t.Fatalf("expected retry to require approval, got %+v", second)
	}
	secondApproval := approvePendingFeeSet(t, restarted, second)
	if secondApproval.Status != "ok" {
		t.Fatalf("failed execution should not persist cooldown, got %+v", secondApproval)
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

func TestDispatchChecksCooldownBeforeQueueingApproval(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
	})
	d := newTestDaemonWithExecutor(t, func(cfg *config.Config) {
		cfg.Approval.RequireApproval = false
		cfg.Limits.PerChannelCooldown = "1h"
	}, fake)

	first := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if first.Status != "pending" {
		t.Fatalf("expected first request to require approval, got %+v", first)
	}
	approved := approvePendingFeeSet(t, d, first)
	if approved.Status != "ok" {
		t.Fatalf("expected approved request to execute, got %+v", approved)
	}

	second := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":120}`),
	})
	if second.Status != "error" || !strings.Contains(second.Reason, "cooldown") {
		t.Fatalf("expected cooldown rejection before approval queue, got %+v",
			second)
	}
	if pending := d.queue.ListPending(); len(pending) != 0 {
		t.Fatalf("cooldown-limited request was queued: %+v", pending)
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
	d := newTestDaemonWithExecutor(t, nil, fake)

	pending := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	if pending.Status != "pending" {
		t.Fatalf("expected pending request, got %+v", pending)
	}

	var once sync.Once
	fake.onCurrent = func() {
		once.Do(func() {
			if err := os.WriteFile(d.cfg.Storage.KillswitchFile, []byte("stop"), 0600); err != nil {
				t.Fatalf("write killswitch: %v", err)
			}
		})
	}

	resp := approvePendingFeeSet(t, d, pending)
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

func TestDispatchRebalanceApprovalExecutesAndLogs(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{})
	d := newTestDaemonWithExecutor(t, nil, fake)

	pending := d.dispatch(Request{
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":1,"incoming_chan_id":2,"amount_sat":200,"max_fee_ppm":100}`),
	})
	if pending.Status != "pending" {
		t.Fatalf("expected pending rebalance, got %+v", pending)
	}

	approved := approvePendingRebalance(t, d, pending)
	if approved.Status != "ok" {
		t.Fatalf("expected approved rebalance ok, got %+v", approved)
	}
	if len(fake.rebalanced) != 1 {
		t.Fatalf("expected one rebalance execution, got %+v", fake.rebalanced)
	}
	got := fake.rebalanced[0]
	if got.OutgoingChanID != 1 || got.IncomingChanID != 2 ||
		got.AmountSat != 200 || got.MaxFeePpm != 100 {

		t.Fatalf("unexpected rebalance request: %+v", got)
	}

	entries, err := d.ledger.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantStatuses := []struct {
		action string
		status string
	}{
		{"execute_rebalance", "pending"},
		{"approve_rebalance", "approved"},
		{"execute_rebalance", "accepted"},
		{"execute_rebalance", "executed"},
	}
	if len(entries) != len(wantStatuses) {
		t.Fatalf("expected %d ledger entries, got %+v",
			len(wantStatuses), entries)
	}
	for i, want := range wantStatuses {
		if entries[i].Action != want.action || entries[i].Status != want.status {
			t.Fatalf("entry %d = %+v, want %s/%s",
				i, entries[i], want.action, want.status)
		}
	}
}

func TestDispatchRebalanceRejectsOverBudgetBeforeQueueing(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{})
	d := newTestDaemonWithExecutor(t, func(cfg *config.Config) {
		cfg.Limits.DailyRebalanceBudgetSat = 100
	}, fake)

	resp := d.dispatch(Request{
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":1,"incoming_chan_id":2,"amount_sat":101,"max_fee_ppm":100}`),
	})
	if resp.Status != "error" ||
		!strings.Contains(resp.Reason, "daily_rebalance_budget_sat") {

		t.Fatalf("expected daily rebalance budget rejection, got %+v", resp)
	}
	if pending := d.queue.ListPending(); len(pending) != 0 {
		t.Fatalf("over-budget rebalance was queued: %+v", pending)
	}
	if len(fake.rebalanced) != 0 {
		t.Fatalf("over-budget rebalance executed: %+v", fake.rebalanced)
	}
}

func TestDispatchRebalanceCooldownCoversBothChannels(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{})
	d := newTestDaemonWithExecutor(t, func(cfg *config.Config) {
		cfg.Limits.PerChannelCooldown = "1h"
	}, fake)

	first := d.dispatch(Request{
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":1,"incoming_chan_id":2,"amount_sat":100,"max_fee_ppm":100}`),
	})
	if first.Status != "pending" {
		t.Fatalf("expected first rebalance to queue, got %+v", first)
	}
	if approved := approvePendingRebalance(t, d, first); approved.Status != "ok" {
		t.Fatalf("expected first rebalance approval ok, got %+v", approved)
	}

	second := d.dispatch(Request{
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":3,"incoming_chan_id":2,"amount_sat":100,"max_fee_ppm":100}`),
	})
	if second.Status != "error" || !strings.Contains(second.Reason, "cooldown") {
		t.Fatalf("expected incoming-channel cooldown rejection, got %+v", second)
	}
}

func TestDispatchRebalanceDailyBudgetSurvivesRestart(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "node-ops")
	if err := os.Mkdir(storeDir, 0700); err != nil {
		t.Fatalf("Mkdir store dir: %v", err)
	}
	cfg := &config.Config{
		Limits: config.Limits{
			DailyRebalanceBudgetSat: 250,
			DailyFeePpmBudget:       500,
			MaxFeePpmDelta:          100,
			PerChannelCooldown:      "0s",
			RebalanceMaxFeePpm:      500,
		},
		Approval: config.Approval{RequireApproval: true},
		Storage: config.Storage{
			LedgerPath:      filepath.Join(storeDir, "ledger.db"),
			LimitsStatePath: filepath.Join(storeDir, "limits-state.json"),
			KillswitchFile:  filepath.Join(storeDir, "STOP"),
		},
	}
	installTestOperatorToken(t, cfg, storeDir)
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{})

	firstDaemon, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New first daemon: %v", err)
	}
	first := firstDaemon.dispatch(Request{
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":1,"incoming_chan_id":2,"amount_sat":200,"max_fee_ppm":100}`),
	})
	if first.Status != "pending" {
		t.Fatalf("expected first rebalance pending, got %+v", first)
	}
	if approved := approvePendingRebalance(t, firstDaemon, first); approved.Status != "ok" {
		t.Fatalf("expected first rebalance approval ok, got %+v", approved)
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
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":3,"incoming_chan_id":4,"amount_sat":51,"max_fee_ppm":100}`),
	})
	if second.Status != "error" ||
		!strings.Contains(second.Reason, "daily_rebalance_budget_sat") {

		t.Fatalf("expected persisted budget rejection, got %+v", second)
	}
}

func TestDispatchRebalanceExecutorFailureRollsBackLimits(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "node-ops")
	if err := os.Mkdir(storeDir, 0700); err != nil {
		t.Fatalf("Mkdir store dir: %v", err)
	}
	cfg := &config.Config{
		Limits: config.Limits{
			DailyRebalanceBudgetSat: 250,
			DailyFeePpmBudget:       500,
			MaxFeePpmDelta:          100,
			PerChannelCooldown:      "1h",
			RebalanceMaxFeePpm:      500,
		},
		Approval: config.Approval{RequireApproval: true},
		Storage: config.Storage{
			LedgerPath:      filepath.Join(storeDir, "ledger.db"),
			LimitsStatePath: filepath.Join(storeDir, "limits-state.json"),
			KillswitchFile:  filepath.Join(storeDir, "STOP"),
		},
	}
	installTestOperatorToken(t, cfg, storeDir)
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{})
	fake.rebalanceErr = errors.New("route failed")

	firstDaemon, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New first daemon: %v", err)
	}
	failed := firstDaemon.dispatch(Request{
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":1,"incoming_chan_id":2,"amount_sat":200,"max_fee_ppm":100}`),
	})
	if failed.Status != "pending" {
		t.Fatalf("expected first rebalance pending, got %+v", failed)
	}
	failedApproval := approvePendingRebalance(t, firstDaemon, failed)
	if failedApproval.Status != "error" ||
		!strings.Contains(failedApproval.Reason, "route failed") {

		t.Fatalf("expected executor failure, got %+v", failedApproval)
	}
	if err := firstDaemon.Close(); err != nil {
		t.Fatalf("Close first daemon: %v", err)
	}

	fake.rebalanceErr = nil
	restarted, err := New(cfg, fake)
	if err != nil {
		t.Fatalf("New restarted daemon: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	second := restarted.dispatch(Request{
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":1,"incoming_chan_id":2,"amount_sat":200,"max_fee_ppm":100}`),
	})
	if second.Status != "pending" {
		t.Fatalf("failed execution should not persist cooldown/budget, got %+v",
			second)
	}
	if approved := approvePendingRebalance(t, restarted, second); approved.Status != "ok" {
		t.Fatalf("retry approval should succeed, got %+v", approved)
	}
	if len(fake.rebalanced) != 1 {
		t.Fatalf("expected one successful rebalance, got %+v", fake.rebalanced)
	}
}

func TestOperatorFeeSetApprovalCannotApproveRebalance(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{})
	d := newTestDaemonWithExecutor(t, nil, fake)

	pending := d.dispatch(Request{
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":1,"incoming_chan_id":2,"amount_sat":100,"max_fee_ppm":100}`),
	})
	if pending.Status != "pending" {
		t.Fatalf("expected pending rebalance, got %+v", pending)
	}
	wrongApproval := d.dispatchOperator(operatorRequest(
		"approve_fee_set",
		mustJSON(t, `{"request_id":"`+pending.RequestID+`"}`),
	))
	if wrongApproval.Status != "error" {
		t.Fatalf("fee-set approval should not approve rebalance, got %+v",
			wrongApproval)
	}
	if pendingItems := d.queue.ListPending(); len(pendingItems) != 1 {
		t.Fatalf("rebalance should remain pending after wrong approval: %+v",
			pendingItems)
	}
	if len(fake.rebalanced) != 0 {
		t.Fatalf("wrong approval executed rebalance: %+v", fake.rebalanced)
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

func TestRunWithOperatorSocketApprovesPendingFeeSet(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		42: {BaseMsat: 1_000, FeePpm: 100},
	})
	d := newTestDaemonWithExecutor(t, nil, fake)
	dir := t.TempDir()
	execSock := filepath.Join(dir, "daemon.sock")
	operatorSock := filepath.Join(dir, "operator.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.RunWithOperator(ctx, execSock, operatorSock)
	}()
	waitForSocket(t, execSock)
	waitForSocket(t, operatorSock)

	pending := socketRoundTrip(t, execSock, Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":42,"base_msat":1000,"fee_ppm":110}`),
	})
	if pending.Status != "pending" {
		t.Fatalf("expected pending response, got %+v", pending)
	}

	approved := socketRoundTrip(t, operatorSock, Request{
		Action:        "approve_fee_set",
		Params:        mustJSON(t, `{"request_id":"`+pending.RequestID+`"}`),
		OperatorToken: testOperatorToken,
	})
	if approved.Status != "ok" {
		t.Fatalf("expected operator approval ok, got %+v", approved)
	}
	if len(fake.executed) != 1 {
		t.Fatalf("expected socket-approved execution, got %+v", fake.executed)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("RunWithOperator: %v", err)
	}
}

func TestRunWithOperatorSocketApprovesPendingRebalance(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{})
	d := newTestDaemonWithExecutor(t, nil, fake)
	dir := t.TempDir()
	execSock := filepath.Join(dir, "daemon.sock")
	operatorSock := filepath.Join(dir, "operator.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.RunWithOperator(ctx, execSock, operatorSock)
	}()
	waitForSocket(t, execSock)
	waitForSocket(t, operatorSock)

	pending := socketRoundTrip(t, execSock, Request{
		Action: "execute_rebalance",
		Params: mustJSON(t, `{"outgoing_chan_id":42,"incoming_chan_id":43,"amount_sat":100,"max_fee_ppm":100}`),
	})
	if pending.Status != "pending" {
		t.Fatalf("expected pending response, got %+v", pending)
	}

	approved := socketRoundTrip(t, operatorSock, Request{
		Action:        "approve_rebalance",
		Params:        mustJSON(t, `{"request_id":"`+pending.RequestID+`"}`),
		OperatorToken: testOperatorToken,
	})
	if approved.Status != "ok" {
		t.Fatalf("expected operator approval ok, got %+v", approved)
	}
	if len(fake.rebalanced) != 1 {
		t.Fatalf("expected socket-approved rebalance, got %+v", fake.rebalanced)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("RunWithOperator: %v", err)
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
			DailyFeePpmBudget:       500,
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
	installTestOperatorToken(t, cfg, storeDir)

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

func TestDispatchChecksDailyFeeBudgetBeforeQueueingApproval(t *testing.T) {
	d := newTestDaemon(t, func(cfg *config.Config) {
		cfg.Limits.DailyFeePpmBudget = 5
	})

	resp := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":42,"base_msat":1000,"fee_ppm":110}`),
	})
	if resp.Status != "error" ||
		!strings.Contains(resp.Reason, "daily_fee_ppm_budget") {

		t.Fatalf("expected daily fee budget rejection, got %+v", resp)
	}
	if pending := d.queue.ListPending(); len(pending) != 0 {
		t.Fatalf("over-budget request was queued: %+v", pending)
	}
}

func TestOperatorApproveExecutesPendingAndLogs(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		42: {BaseMsat: 1_000, FeePpm: 100},
	})
	d := newTestDaemonWithExecutor(t, nil, fake)

	pending := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":42,"base_msat":1000,"fee_ppm":110}`),
	})
	if pending.Status != "pending" {
		t.Fatalf("expected pending response, got %+v", pending)
	}

	approved := approvePendingFeeSet(t, d, pending)
	if approved.Status != "ok" {
		t.Fatalf("expected approval execution ok, got %+v", approved)
	}
	if len(fake.executed) != 1 {
		t.Fatalf("expected one executed fee set, got %+v", fake.executed)
	}
	if got := fake.executed[0]; got.ChanID != 42 || got.BaseMsat != 1_000 ||
		got.FeePpm != 110 {

		t.Fatalf("unexpected executed request: %+v", got)
	}

	entries, err := d.ledger.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantStatuses := []struct {
		action string
		status string
	}{
		{"execute_fee_set", "pending"},
		{"approve_fee_set", "approved"},
		{"execute_fee_set", "accepted"},
		{"execute_fee_set", "executed"},
	}
	if len(entries) != len(wantStatuses) {
		t.Fatalf("expected %d ledger entries, got %+v",
			len(wantStatuses), entries)
	}
	for i, want := range wantStatuses {
		if entries[i].Action != want.action || entries[i].Status != want.status {
			t.Fatalf("entry %d = %+v, want %s/%s",
				i, entries[i], want.action, want.status)
		}
	}
}

func TestOperatorApproveRechecksDailyFeeBudgetBeforeExecution(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		1: {BaseMsat: 1_000, FeePpm: 100},
		2: {BaseMsat: 1_000, FeePpm: 100},
	})
	d := newTestDaemonWithExecutor(t, func(cfg *config.Config) {
		cfg.Limits.DailyFeePpmBudget = 15
	}, fake)

	first := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":1,"base_msat":1000,"fee_ppm":110}`),
	})
	second := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":2,"base_msat":1000,"fee_ppm":110}`),
	})
	if first.Status != "pending" || second.Status != "pending" {
		t.Fatalf("expected both requests to queue, got first=%+v second=%+v",
			first, second)
	}

	firstApproval := approvePendingFeeSet(t, d, first)
	if firstApproval.Status != "ok" {
		t.Fatalf("expected first approval to execute, got %+v", firstApproval)
	}
	secondApproval := approvePendingFeeSet(t, d, second)
	if secondApproval.Status != "error" ||
		!strings.Contains(secondApproval.Reason, "daily_fee_ppm_budget") {

		t.Fatalf("expected second approval budget rejection, got %+v",
			secondApproval)
	}
	if len(fake.executed) != 1 || fake.executed[0].ChanID != 1 {
		t.Fatalf("expected only first request to execute, got %+v",
			fake.executed)
	}
}

func TestOperatorDenyDoesNotExecuteAndLogs(t *testing.T) {
	fake := newFakeExecutor(map[uint64]executor.FeePolicy{
		42: {BaseMsat: 1_000, FeePpm: 100},
	})
	d := newTestDaemonWithExecutor(t, nil, fake)

	pending := d.dispatch(Request{
		Action: "execute_fee_set",
		Params: mustJSON(t, `{"chan_id":42,"base_msat":1000,"fee_ppm":110}`),
	})
	if pending.Status != "pending" {
		t.Fatalf("expected pending response, got %+v", pending)
	}

	denied := d.dispatchOperator(operatorRequest(
		"deny_fee_set",
		mustJSON(t, `{"request_id":"`+pending.RequestID+`","reason":"not today"}`),
	))
	if denied.Status != "ok" {
		t.Fatalf("expected denial ok, got %+v", denied)
	}
	if len(fake.executed) != 0 {
		t.Fatalf("denied request executed: %+v", fake.executed)
	}
	if pending := d.queue.ListPending(); len(pending) != 0 {
		t.Fatalf("denied request still pending: %+v", pending)
	}

	entries, err := d.ledger.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantStatuses := []struct {
		action string
		status string
		reason string
	}{
		{"execute_fee_set", "pending", ""},
		{"deny_fee_set", "denied", "not today"},
		{"execute_fee_set", "denied", "not today"},
	}
	if len(entries) != len(wantStatuses) {
		t.Fatalf("expected %d ledger entries, got %+v",
			len(wantStatuses), entries)
	}
	for i, want := range wantStatuses {
		if entries[i].Action != want.action || entries[i].Status != want.status ||
			entries[i].Reason != want.reason {

			t.Fatalf("entry %d = %+v, want %s/%s/%q",
				i, entries[i], want.action, want.status, want.reason)
		}
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

	resp = d.dispatch(Request{
		Action: "approve_fee_set",
		Params: mustJSON(t, `{"request_id":"pending-id"}`),
	})
	if resp.Status != "error" || !strings.Contains(resp.Reason, "unknown action") {
		t.Fatalf("expected unknown action error, got %+v", resp)
	}
}

func TestOperatorSocketRequiresToken(t *testing.T) {
	d := newTestDaemon(t, nil)

	missing := d.dispatchOperator(Request{
		Action: "approve_fee_set",
		Params: mustJSON(t, `{"request_id":"pending-id"}`),
	})
	if missing.Status != "error" ||
		!strings.Contains(missing.Reason, "missing operator token") {

		t.Fatalf("expected missing token rejection, got %+v", missing)
	}

	invalid := d.dispatchOperator(Request{
		Action:        "approve_fee_set",
		Params:        mustJSON(t, `{"request_id":"pending-id"}`),
		OperatorToken: "wrong-token",
	})
	if invalid.Status != "error" ||
		!strings.Contains(invalid.Reason, "invalid operator token") {

		t.Fatalf("expected invalid token rejection, got %+v", invalid)
	}
	if pending := d.queue.ListPending(); len(pending) != 0 {
		t.Fatalf("token failures should not modify queue: %+v", pending)
	}
}
