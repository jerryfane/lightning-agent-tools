// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package monitor polls node health and pushes deduplicated alerts to an
// operator-configured channel.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/executor"
)

// HealthReader supplies read-only node health snapshots.
type HealthReader interface {
	NodeHealth(ctx context.Context) (executor.NodeHealthSnapshot, error)
}

// Publisher sends alert events to a configured push channel.
type Publisher interface {
	Publish(ctx context.Context, event AlertEvent) error
}

// Config controls monitor polling and duplicate suppression.
type Config struct {
	PollInterval  time.Duration
	AlertCooldown time.Duration
}

// AlertEvent is the pushed alert payload.
type AlertEvent struct {
	ID            string                 `json:"id"`
	FiredAt       time.Time              `json:"fired_at"`
	Severity      string                 `json:"severity"`
	Category      string                 `json:"category"`
	Message       string                 `json:"message"`
	NodeID        string                 `json:"node_id,omitempty"`
	Alias         string                 `json:"alias,omitempty"`
	OverallStatus string                 `json:"overall_status,omitempty"`
	Details       map[string]interface{} `json:"details,omitempty"`
}

// Monitor polls node health and publishes newly observed alerts.
type Monitor struct {
	reader    HealthReader
	publisher Publisher
	cfg       Config

	mu        sync.Mutex
	lastSent  map[string]time.Time
	lastErr   error
	lastErrAt time.Time
	now       func() time.Time
}

// New creates a health monitor.
func New(reader HealthReader, publisher Publisher, cfg Config) (*Monitor, error) {
	if reader == nil {
		return nil, fmt.Errorf("health reader must not be nil")
	}
	if publisher == nil {
		return nil, fmt.Errorf("alert publisher must not be nil")
	}
	if cfg.PollInterval <= 0 {
		return nil, fmt.Errorf("poll interval must be positive")
	}
	if cfg.AlertCooldown < 0 {
		return nil, fmt.Errorf("alert cooldown must be non-negative")
	}
	return &Monitor{
		reader:    reader,
		publisher: publisher,
		cfg:       cfg,
		lastSent:  make(map[string]time.Time),
		now:       time.Now,
	}, nil
}

// SetClock replaces the monitor clock. It is intended for deterministic tests.
func (m *Monitor) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// LastError returns the most recent polling or publishing error.
func (m *Monitor) LastError() (string, time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastErr == nil {
		return "", time.Time{}, false
	}
	return m.lastErr.Error(), m.lastErrAt, true
}

// Run polls immediately and then on the configured interval until ctx is done.
func (m *Monitor) Run(ctx context.Context) {
	m.pollAndRecord(ctx)

	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollAndRecord(ctx)
		}
	}
}

func (m *Monitor) pollAndRecord(ctx context.Context) {
	m.recordError(m.Poll(ctx))
}

func (m *Monitor) recordError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastErr = err
	if err != nil {
		m.lastErrAt = m.now().UTC()
		return
	}
	m.lastErrAt = time.Time{}
}

// Poll reads node health once and publishes every non-suppressed alert.
func (m *Monitor) Poll(ctx context.Context) error {
	snapshot, err := m.reader.NodeHealth(ctx)
	if err != nil {
		if publishErr := m.publish(ctx, AlertEvent{
			ID:       "monitor:node_health_poll_failed",
			FiredAt:  m.currentTime(),
			Severity: "warning",
			Category: "monitor",
			Message:  "Node health poll failed",
			Details: map[string]interface{}{
				"error": err.Error(),
			},
		}); publishErr != nil {
			return publishErr
		}
		return fmt.Errorf("node health poll failed: %w", err)
	}

	for _, alert := range snapshot.Alerts {
		event := alertEvent(snapshot, alert, m.currentTime())
		if err := m.publish(ctx, event); err != nil {
			return err
		}
	}
	return nil
}

func (m *Monitor) publish(ctx context.Context, event AlertEvent) error {
	if event.ID == "" {
		event.ID = "monitor:unknown"
	}
	if !m.shouldPublish(event.ID, event.FiredAt) {
		return nil
	}
	if err := m.publisher.Publish(ctx, event); err != nil {
		return err
	}
	m.recordPublish(event.ID, event.FiredAt)
	return nil
}

func (m *Monitor) shouldPublish(id string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	last, ok := m.lastSent[id]
	if ok && m.cfg.AlertCooldown > 0 && now.Sub(last) < m.cfg.AlertCooldown {
		return false
	}
	return true
}

func (m *Monitor) recordPublish(id string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSent[id] = now
}

func (m *Monitor) currentTime() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now().UTC()
}

func alertEvent(snapshot executor.NodeHealthSnapshot,
	alert executor.HealthAlert, firedAt time.Time) AlertEvent {

	id := alert.ID
	if id == "" {
		id = alertFingerprint(alert)
	}
	return AlertEvent{
		ID:            id,
		FiredAt:       firedAt,
		Severity:      alert.Severity,
		Category:      alert.Category,
		Message:       alert.Message,
		NodeID:        snapshot.NodeID,
		Alias:         snapshot.Alias,
		OverallStatus: snapshot.OverallStatus,
		Details:       alert.Details,
	}
}

func alertFingerprint(alert executor.HealthAlert) string {
	parts := []string{alert.Severity, alert.Category, alert.Message}
	for _, key := range []string{
		"channel_point", "remote_node_pub", "closing_txid", "pub_key",
	} {
		if value, ok := alert.Details[key]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", key, value))
		}
	}
	return strings.Join(parts, "|")
}

// JSONLPublisher appends alerts to a private JSONL file.
type JSONLPublisher struct {
	path string
	mu   sync.Mutex
}

// NewJSONLPublisher creates a JSONL alert publisher.
func NewJSONLPublisher(path string) (*JSONLPublisher, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("alert path must not be empty")
	}
	if err := prepareAlertDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := prepareAlertFile(path); err != nil {
		return nil, err
	}
	return &JSONLPublisher{path: path}, nil
}

func prepareAlertDir(dir string) error {
	if _, err := os.Lstat(dir); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat alert dir: %w", err)
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create alert dir: %w", err)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat alert dir: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("alert dir %s must not be a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("alert path parent %s is not a directory", dir)
	}
	if err := checkAlertDirOwner(dir, info); err != nil {
		return err
	}
	if info.Mode().Perm()&0077 == 0 {
		if info.Mode().Perm()&0300 == 0300 {
			return nil
		}
		return fmt.Errorf("alert dir %s must be owner-writable and searchable",
			dir)
	}
	return fmt.Errorf("alert dir %s has unsafe permissions %03o",
		dir, info.Mode().Perm())
}

func prepareAlertFile(path string) error {
	f, err := openSecureAlertFile(path)
	if err != nil {
		return err
	}
	return f.Close()
}

func openSecureAlertFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat alert file: %w", err)
	}
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("alert file %s must not be a symlink", path)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("alert file %s is not a regular file", path)
		}
	}
	f, err := openAlertFileNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("open alert file: %w", err)
	}
	fileInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat alert file: %w", err)
	}
	if !fileInfo.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("alert file %s is not a regular file", path)
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure alert file: %w", err)
	}
	return f, nil
}

// Publish appends event as one JSON object.
func (p *JSONLPublisher) Publish(ctx context.Context, event AlertEvent) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := prepareAlertDir(filepath.Dir(p.path)); err != nil {
		return err
	}
	f, err := openSecureAlertFile(p.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(event); err != nil {
		return fmt.Errorf("write alert: %w", err)
	}
	return nil
}

// WriterPublisher writes JSONL alerts to an io.Writer such as stdout.
type WriterPublisher struct {
	w  io.Writer
	mu sync.Mutex
}

// NewWriterPublisher creates a JSONL writer publisher.
func NewWriterPublisher(w io.Writer) (*WriterPublisher, error) {
	if w == nil {
		return nil, fmt.Errorf("alert writer must not be nil")
	}
	return &WriterPublisher{w: w}, nil
}

// Publish writes event as one JSON object.
func (p *WriterPublisher) Publish(ctx context.Context, event AlertEvent) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := json.NewEncoder(p.w).Encode(event); err != nil {
		return fmt.Errorf("write alert: %w", err)
	}
	return nil
}
