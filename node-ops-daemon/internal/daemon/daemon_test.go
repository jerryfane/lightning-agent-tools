// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/executor"
)

func newTestDaemon(t *testing.T, mutate func(*config.Config)) *Daemon {
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

	d, err := New(cfg, &executor.StubExecutor{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
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
