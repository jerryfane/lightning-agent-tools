// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/executor"
)

type fakeHealthReader struct {
	snapshot executor.NodeHealthSnapshot
	err      error
}

func (f *fakeHealthReader) NodeHealth(context.Context) (executor.NodeHealthSnapshot, error) {
	return f.snapshot, f.err
}

type capturePublisher struct {
	events []AlertEvent
	err    error
}

func (p *capturePublisher) Publish(_ context.Context, event AlertEvent) error {
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, event)
	return nil
}

func newTestMonitor(t *testing.T, reader HealthReader,
	publisher Publisher) *Monitor {

	t.Helper()
	mon, err := New(reader, publisher, Config{
		PollInterval:  time.Second,
		AlertCooldown: time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mon
}

func forceCloseSnapshot() executor.NodeHealthSnapshot {
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

func TestPollPublishesForceCloseAlert(t *testing.T) {
	pub := &capturePublisher{}
	mon := newTestMonitor(t, &fakeHealthReader{
		snapshot: forceCloseSnapshot(),
	}, pub)
	firedAt := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	mon.SetClock(func() time.Time { return firedAt })

	if err := mon.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if len(pub.events) != 1 {
		t.Fatalf("expected one alert, got %d", len(pub.events))
	}
	event := pub.events[0]
	if event.ID != "channel:force-close:txid:0" {
		t.Fatalf("event ID = %q", event.ID)
	}
	if event.Severity != "critical" || event.Category != "channel" ||
		!strings.Contains(event.Message, "Force-closing") {

		t.Fatalf("unexpected event: %+v", event)
	}
	if event.NodeID != "node-123" || event.Alias != "regtest-node" ||
		event.OverallStatus != "critical" {

		t.Fatalf("snapshot fields were not propagated: %+v", event)
	}
}

func TestPollSuppressesDuplicateUntilCooldownExpires(t *testing.T) {
	pub := &capturePublisher{}
	mon := newTestMonitor(t, &fakeHealthReader{
		snapshot: forceCloseSnapshot(),
	}, pub)
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	mon.SetClock(func() time.Time { return now })

	if err := mon.Poll(context.Background()); err != nil {
		t.Fatalf("first Poll: %v", err)
	}
	if err := mon.Poll(context.Background()); err != nil {
		t.Fatalf("second Poll: %v", err)
	}
	if len(pub.events) != 1 {
		t.Fatalf("duplicate inside cooldown should be suppressed, got %d events",
			len(pub.events))
	}

	now = now.Add(time.Minute)
	if err := mon.Poll(context.Background()); err != nil {
		t.Fatalf("third Poll: %v", err)
	}
	if len(pub.events) != 2 {
		t.Fatalf("alert after cooldown should publish again, got %d events",
			len(pub.events))
	}
}

func TestPollRetriesAfterPublishFailure(t *testing.T) {
	pub := &capturePublisher{err: errors.New("disk full")}
	mon := newTestMonitor(t, &fakeHealthReader{
		snapshot: forceCloseSnapshot(),
	}, pub)

	if err := mon.Poll(context.Background()); err == nil {
		t.Fatalf("expected publish error")
	}
	pub.err = nil
	if err := mon.Poll(context.Background()); err != nil {
		t.Fatalf("retry Poll: %v", err)
	}
	if len(pub.events) != 1 {
		t.Fatalf("failed publish should not consume cooldown, got %d events",
			len(pub.events))
	}
}

func TestPollPublishesHealthPollFailureAlert(t *testing.T) {
	pub := &capturePublisher{}
	mon := newTestMonitor(t, &fakeHealthReader{
		err: errors.New("lnd unavailable"),
	}, pub)

	if err := mon.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(pub.events) != 1 {
		t.Fatalf("expected poll failure alert, got %d", len(pub.events))
	}
	event := pub.events[0]
	if event.ID != "monitor:node_health_poll_failed" ||
		event.Category != "monitor" || event.Severity != "warning" {

		t.Fatalf("unexpected poll failure alert: %+v", event)
	}
	if event.Details["error"] != "lnd unavailable" {
		t.Fatalf("expected poll error detail, got %+v", event.Details)
	}
}

func TestWriterPublisherWritesJSONL(t *testing.T) {
	var buf bytes.Buffer
	pub, err := NewWriterPublisher(&buf)
	if err != nil {
		t.Fatalf("NewWriterPublisher: %v", err)
	}

	event := AlertEvent{
		ID:       "peer:flap:abc",
		FiredAt:  time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC),
		Severity: "warning",
		Category: "peer",
		Message:  "Peer has high flap count",
	}
	if err := pub.Publish(context.Background(), event); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	var decoded AlertEvent
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &decoded); err != nil {
		t.Fatalf("decode JSONL: %v", err)
	}
	if decoded.ID != event.ID || decoded.Message != event.Message {
		t.Fatalf("decoded event = %+v, want %+v", decoded, event)
	}
}

func TestJSONLPublisherCreatesPrivateAlertFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alerts", "alerts.jsonl")
	pub, err := NewJSONLPublisher(path)
	if err != nil {
		t.Fatalf("NewJSONLPublisher: %v", err)
	}
	if err := pub.Publish(context.Background(), AlertEvent{
		ID:       "channel:force-close",
		FiredAt:  time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC),
		Severity: "critical",
		Category: "channel",
		Message:  "Force-closing channel detected",
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat alert file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("alert file mode = %03o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat alert dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("alert dir mode = %03o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestJSONLPublisherRejectsUnsafeExistingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "alerts")
	if err := os.Mkdir(dir, 0777); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	_, err := NewJSONLPublisher(filepath.Join(dir, "alerts.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("expected unsafe directory rejection, got %v", err)
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		t.Fatalf("Stat: %v", statErr)
	}
	if info.Mode().Perm()&0077 == 0 {
		t.Fatalf("publisher chmodded existing unsafe directory to %03o",
			info.Mode().Perm())
	}
}

func TestJSONLPublisherRejectsUnwritablePrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "alerts")
	if err := os.Mkdir(dir, 0500); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	_, err := NewJSONLPublisher(filepath.Join(dir, "alerts.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "owner-writable") {
		t.Fatalf("expected unwritable directory rejection, got %v", err)
	}
}
