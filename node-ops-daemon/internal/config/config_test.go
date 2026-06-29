// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPartialConfigPreservesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	err := os.WriteFile(path, []byte(`
[limits]
max_fee_ppm_delta = 42
`), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Limits.MaxFeePpmDelta != 42 {
		t.Fatalf("expected configured ppm delta, got %d", cfg.Limits.MaxFeePpmDelta)
	}
	if !cfg.Approval.RequireApproval {
		t.Fatalf("partial config should preserve require_approval default")
	}
	if cfg.Storage.LedgerPath == "" || cfg.Storage.LimitsStatePath == "" ||
		cfg.Storage.KillswitchFile == "" {
		t.Fatalf("partial config should preserve storage defaults: %+v", cfg.Storage)
	}
	if cfg.Monitor.Enabled || cfg.Monitor.PollInterval == "" ||
		cfg.Monitor.AlertCooldown == "" || cfg.Monitor.AlertPath == "" {

		t.Fatalf("partial config should preserve monitor defaults: %+v", cfg.Monitor)
	}
}

func TestLoadExpandsStoragePaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	err := os.WriteFile(path, []byte(`
[storage]
ledger = "~/.node-ops/custom-ledger.db"
limits_state = "~/.node-ops/custom-limits-state.json"
killswitch = "~/.node-ops/CUSTOM_STOP"
`), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if want := filepath.Join(home, ".node-ops", "custom-ledger.db"); cfg.Storage.LedgerPath != want {
		t.Fatalf("ledger path = %q, want %q", cfg.Storage.LedgerPath, want)
	}
	if want := filepath.Join(home, ".node-ops", "custom-limits-state.json"); cfg.Storage.LimitsStatePath != want {
		t.Fatalf("limits state path = %q, want %q", cfg.Storage.LimitsStatePath, want)
	}
	if want := filepath.Join(home, ".node-ops", "CUSTOM_STOP"); cfg.Storage.KillswitchFile != want {
		t.Fatalf("killswitch path = %q, want %q", cfg.Storage.KillswitchFile, want)
	}
}

func TestLoadExpandsMonitorAlertPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	err := os.WriteFile(path, []byte(`
[monitor]
alert_path = "~/.node-ops/custom-alerts.jsonl"
`), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := filepath.Join(home, ".node-ops", "custom-alerts.jsonl")
	if cfg.Monitor.AlertPath != want {
		t.Fatalf("alert path = %q, want %q", cfg.Monitor.AlertPath, want)
	}
}

func TestLoadRejectsEmptyStoragePaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "ledger",
			body: `
	[storage]
	ledger = ""
	`,
			wantErr: "storage.ledger",
		},
		{
			name: "limits_state",
			body: `
	[storage]
	limits_state = ""
	`,
			wantErr: "storage.limits_state",
		},
		{
			name: "killswitch",
			body: `
	[storage]
	killswitch = " "
	`,
			wantErr: "storage.killswitch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q error, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadRejectsInvalidMonitorConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "bad poll interval",
			body: `
[monitor]
poll_interval = "nope"
`,
			wantErr: "monitor.poll_interval",
		},
		{
			name: "zero poll interval",
			body: `
[monitor]
poll_interval = "0s"
`,
			wantErr: "must be positive",
		},
		{
			name: "negative cooldown",
			body: `
[monitor]
alert_cooldown = "-1s"
`,
			wantErr: "monitor.alert_cooldown",
		},
		{
			name: "unknown channel",
			body: `
[monitor]
alert_channel = "webhook"
`,
			wantErr: "monitor.alert_channel",
		},
		{
			name: "empty file path",
			body: `
[monitor]
alert_channel = "file"
alert_path = ""
`,
			wantErr: "monitor.alert_path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q error, got %v", tc.wantErr, err)
			}
		})
	}
}
