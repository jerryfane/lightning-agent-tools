// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package ledger_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/ledger"
)

func openTemp(t *testing.T) *ledger.Ledger {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestRecord_SingleEntry(t *testing.T) {
	l := openTemp(t)

	e := ledger.Entry{
		RequestID: "req-001",
		Action:    "execute_fee_set",
		Params:    `{"chan_id":1,"fee_ppm":100}`,
		Status:    "executed",
		CreatedAt: time.Now().UTC(),
	}
	if err := l.Record(e); err != nil {
		t.Fatalf("Record: %v", err)
	}

	entries, err := l.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].RequestID != "req-001" {
		t.Errorf("request_id mismatch: %s", entries[0].RequestID)
	}
	if entries[0].Status != "executed" {
		t.Errorf("status mismatch: %s", entries[0].Status)
	}
}

func TestRecord_AppendOnly(t *testing.T) {
	l := openTemp(t)

	for i := 0; i < 5; i++ {
		if err := l.Record(ledger.Entry{
			RequestID: "req",
			Action:    "execute_fee_set",
			Status:    "pending",
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	entries, err := l.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries (append-only), got %d", len(entries))
	}
}

func TestRecord_ConcurrentWrites(t *testing.T) {
	l := openTemp(t)

	const goroutines = 16
	const writesPerGoroutine = 20

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*writesPerGoroutine)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < writesPerGoroutine; j++ {
				err := l.Record(ledger.Entry{
					RequestID: fmt.Sprintf("req-%d-%d", worker, j),
					Action:    "status",
					Status:    "ok",
					CreatedAt: time.Now().UTC(),
				})
				if err != nil {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("Record returned error during concurrent writes: %v", err)
	}

	entries, err := l.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	expected := goroutines * writesPerGoroutine
	if len(entries) != expected {
		t.Fatalf("expected %d entries, got %d", expected, len(entries))
	}
}

func TestRecord_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(ledger.Entry{
		RequestID: "persisted",
		Action:    "execute_fee_set",
		Status:    "executed",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()

	entries, err := l2.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].RequestID != "persisted" {
		t.Fatalf("expected persisted entry after reopen, got %v", entries)
	}
}

func TestRecord_EmptyParams(t *testing.T) {
	l := openTemp(t)
	if err := l.Record(ledger.Entry{
		RequestID: "r1",
		Action:    "status",
		Status:    "ok",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record with empty params: %v", err)
	}

	entries, _ := l.List()
	if entries[0].Params != "{}" {
		t.Errorf("expected '{}' default, got %q", entries[0].Params)
	}
}

// Ensure the ledger file can be created even if the directory already exists
// (common use-case: daemon creates ~/.node-ops then opens the ledger).
func TestOpen_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.db")

	l, err := ledger.Open(path)
	if err != nil {
		t.Fatalf("Open on new path: %v", err)
	}
	l.Close()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file to exist after Open: %v", err)
	}
}
