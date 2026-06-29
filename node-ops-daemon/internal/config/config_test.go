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
	if cfg.Limits.DailyFeePpmBudget == 0 {
		t.Fatalf("partial config should preserve daily fee ppm budget default")
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
	if cfg.Node.LndRPC == "" || cfg.Node.MacaroonPath == "" ||
		cfg.Node.TLSCertPath == "" || cfg.Node.RequiredNetwork != "regtest" {

		t.Fatalf("partial config should preserve node defaults: %+v", cfg.Node)
	}
	if cfg.Operator.ApprovalSocket == "" {
		t.Fatalf("partial config should preserve operator defaults: %+v", cfg.Operator)
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

func TestLoadExpandsNodeAndOperatorPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	err := os.WriteFile(path, []byte(`
[node]
macaroon = "~/.node-ops/custom-node-ops.macaroon"
tls_cert = "~/.lnd/custom-tls.cert"

[operator]
approval_socket = "~/.node-ops/custom-operator.sock"
approval_token_file = "~/.node-ops/custom-operator.token"
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

	if want := filepath.Join(home, ".node-ops", "custom-node-ops.macaroon"); cfg.Node.MacaroonPath != want {
		t.Fatalf("macaroon path = %q, want %q", cfg.Node.MacaroonPath, want)
	}
	if want := filepath.Join(home, ".lnd", "custom-tls.cert"); cfg.Node.TLSCertPath != want {
		t.Fatalf("tls cert path = %q, want %q", cfg.Node.TLSCertPath, want)
	}
	if want := filepath.Join(home, ".node-ops", "custom-operator.sock"); cfg.Operator.ApprovalSocket != want {
		t.Fatalf("operator socket = %q, want %q", cfg.Operator.ApprovalSocket, want)
	}
	if want := filepath.Join(home, ".node-ops", "custom-operator.token"); cfg.Operator.ApprovalTokenFile != want {
		t.Fatalf("operator token file = %q, want %q",
			cfg.Operator.ApprovalTokenFile, want)
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

func TestLoadRejectsInvalidLimitsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	err := os.WriteFile(path, []byte(`
[limits]
daily_fee_ppm_budget = -1
`), 0600)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err = Load(path)
	if err == nil || !strings.Contains(err.Error(), "daily_fee_ppm_budget") {
		t.Fatalf("expected daily fee budget error, got %v", err)
	}
}

func TestLoadRejectsInvalidNodeAndOperatorConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "lnd rpc",
			body: `
[node]
lnd_rpc = ""
`,
			wantErr: "node.lnd_rpc",
		},
		{
			name: "macaroon",
			body: `
[node]
macaroon = ""
`,
			wantErr: "node.macaroon",
		},
		{
			name: "tls cert",
			body: `
[node]
tls_cert = " "
`,
			wantErr: "node.tls_cert",
		},
		{
			name: "required network",
			body: `
[node]
required_network = ""
`,
			wantErr: "node.required_network",
		},
		{
			name: "mainnet rejected",
			body: `
[node]
required_network = "mainnet"
`,
			wantErr: "node.required_network",
		},
		{
			name: "dial timeout",
			body: `
[node]
dial_timeout = "0s"
`,
			wantErr: "node.dial_timeout",
		},
		{
			name: "request timeout",
			body: `
[node]
request_timeout = "nope"
`,
			wantErr: "node.request_timeout",
		},
		{
			name: "operator socket",
			body: `
[operator]
approval_socket = " "
`,
			wantErr: "operator.approval_socket",
		},
		{
			name: "operator token file",
			body: `
[operator]
approval_token_file = " "
`,
			wantErr: "operator.approval_token_file",
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
