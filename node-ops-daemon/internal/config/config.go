// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Limits controls how much the daemon is allowed to do autonomously.
type Limits struct {
	// DailyRebalanceBudgetSat is the max sats that may be rebalanced per UTC day.
	DailyRebalanceBudgetSat int64 `toml:"daily_rebalance_budget_sat"`

	// DailyFeePpmBudget is the max cumulative absolute fee-rate delta, in ppm,
	// that may be executed per UTC day.
	DailyFeePpmBudget int64 `toml:"daily_fee_ppm_budget"`

	// MaxFeePpmDelta is the largest single fee change (absolute, in ppm) the
	// daemon may execute.
	MaxFeePpmDelta int64 `toml:"max_fee_ppm_delta"`

	// PerChannelCooldown is a Go duration string (e.g. "1h") enforcing a
	// minimum interval between successive operations on the same channel.
	PerChannelCooldown string `toml:"per_channel_cooldown"`

	// RebalanceMaxFeePpm is the ceiling fee rate (ppm) for any rebalance route.
	RebalanceMaxFeePpm int64 `toml:"rebalance_max_fee_ppm"`
}

// Approval controls pending-queue policy for money-affecting actions.
type Approval struct {
	// AutoExecuteBelowPpmDelta is reserved for future action types. Fee-set
	// execution always requires operator approval.
	AutoExecuteBelowPpmDelta int64 `toml:"auto_execute_below_ppm_delta"`

	// RequireApproval forces supported actions into the pending queue regardless
	// of size. Fee-set execution is always queued.
	RequireApproval bool `toml:"require_approval"`
}

// Storage holds filesystem paths.
type Storage struct {
	// LedgerPath is the path to the SQLite audit ledger.
	LedgerPath string `toml:"ledger"`

	// LimitsStatePath is the path to the persisted limits engine state.
	LimitsStatePath string `toml:"limits_state"`

	// KillswitchFile is the path whose mere presence halts all execution.
	KillswitchFile string `toml:"killswitch"`
}

// Node holds the daemon-owned LND connection and scoped macaroon settings.
type Node struct {
	// LndRPC is the host:port of the LND gRPC endpoint.
	LndRPC string `toml:"lnd_rpc"`

	// MacaroonPath is the node-ops scoped macaroon loaded only by the daemon.
	MacaroonPath string `toml:"macaroon"`

	// TLSCertPath is the TLS certificate used to authenticate the LND endpoint.
	TLSCertPath string `toml:"tls_cert"`

	// RequiredNetwork is the only LND network this first write path may use.
	RequiredNetwork string `toml:"required_network"`

	// DialTimeout bounds startup connection attempts.
	DialTimeout string `toml:"dial_timeout"`

	// RequestTimeout bounds individual LND RPCs.
	RequestTimeout string `toml:"request_timeout"`
}

// Operator holds the human/operator-only approval boundary.
type Operator struct {
	// ApprovalSocket is the separate local socket for approve/deny actions.
	ApprovalSocket string `toml:"approval_socket"`

	// ApprovalTokenFile is a human/operator-only token required on the
	// operator socket before approve/deny actions are accepted.
	ApprovalTokenFile string `toml:"approval_token_file"`
}

// Monitor controls background read-only node health polling and alert output.
type Monitor struct {
	// Enabled starts the background monitor when the daemon starts.
	Enabled bool `toml:"enabled"`

	// PollInterval is a Go duration string for node_health polling.
	PollInterval string `toml:"poll_interval"`

	// AlertCooldown suppresses duplicate alert pushes for this duration.
	AlertCooldown string `toml:"alert_cooldown"`

	// AlertChannel selects where alerts are pushed: "file" or "stdout".
	AlertChannel string `toml:"alert_channel"`

	// AlertPath is the JSONL destination when AlertChannel is "file".
	AlertPath string `toml:"alert_path"`
}

// Config is the root configuration for the daemon.
type Config struct {
	Limits   Limits   `toml:"limits"`
	Approval Approval `toml:"approval"`
	Storage  Storage  `toml:"storage"`
	Node     Node     `toml:"node"`
	Operator Operator `toml:"operator"`
	Monitor  Monitor  `toml:"monitor"`
}

// DefaultPath returns the canonical config path for the current user.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".node-ops", "config.toml")
}

// Defaults returns a safe, permissive configuration used when no file exists.
func Defaults() *Config {
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".node-ops")
	return &Config{
		Limits: Limits{
			DailyRebalanceBudgetSat: 1_000_000,
			DailyFeePpmBudget:       500,
			MaxFeePpmDelta:          100,
			PerChannelCooldown:      "1h",
			RebalanceMaxFeePpm:      500,
		},
		Approval: Approval{
			AutoExecuteBelowPpmDelta: 20,
			RequireApproval:          true,
		},
		Storage: Storage{
			LedgerPath:      filepath.Join(base, "ledger.db"),
			LimitsStatePath: filepath.Join(base, "limits-state.json"),
			KillswitchFile:  filepath.Join(base, "STOP"),
		},
		Node: Node{
			LndRPC:          "127.0.0.1:10009",
			MacaroonPath:    filepath.Join(base, "node-ops.macaroon"),
			TLSCertPath:     filepath.Join(home, ".lnd", "tls.cert"),
			RequiredNetwork: "regtest",
			DialTimeout:     "5s",
			RequestTimeout:  "10s",
		},
		Operator: Operator{
			ApprovalSocket:    filepath.Join(base, "operator.sock"),
			ApprovalTokenFile: filepath.Join(base, "operator.token"),
		},
		Monitor: Monitor{
			Enabled:       false,
			PollInterval:  "30s",
			AlertCooldown: "10m",
			AlertChannel:  "file",
			AlertPath:     filepath.Join(base, "alerts.jsonl"),
		},
	}
}

// Load parses a TOML config file and returns a Config.
func Load(path string) (*Config, error) {
	c := Defaults()
	if _, err := toml.DecodeFile(path, c); err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	if err := c.expand(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// expand resolves ~ in storage paths.
func (c *Config) expand() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	c.Storage.LedgerPath = expandHome(c.Storage.LedgerPath, home)
	c.Storage.LimitsStatePath = expandHome(c.Storage.LimitsStatePath, home)
	c.Storage.KillswitchFile = expandHome(c.Storage.KillswitchFile, home)
	c.Node.MacaroonPath = expandHome(c.Node.MacaroonPath, home)
	c.Node.TLSCertPath = expandHome(c.Node.TLSCertPath, home)
	c.Operator.ApprovalSocket = expandHome(c.Operator.ApprovalSocket, home)
	c.Operator.ApprovalTokenFile = expandHome(c.Operator.ApprovalTokenFile, home)
	c.Monitor.AlertPath = expandHome(c.Monitor.AlertPath, home)
	return nil
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Storage.LedgerPath) == "" {
		return fmt.Errorf("storage.ledger must not be empty")
	}
	if strings.TrimSpace(c.Storage.LimitsStatePath) == "" {
		return fmt.Errorf("storage.limits_state must not be empty")
	}
	if strings.TrimSpace(c.Storage.KillswitchFile) == "" {
		return fmt.Errorf("storage.killswitch must not be empty")
	}
	if c.Limits.DailyFeePpmBudget < 0 {
		return fmt.Errorf("limits.daily_fee_ppm_budget must be non-negative")
	}
	if strings.TrimSpace(c.Node.LndRPC) == "" {
		return fmt.Errorf("node.lnd_rpc must not be empty")
	}
	if strings.TrimSpace(c.Node.MacaroonPath) == "" {
		return fmt.Errorf("node.macaroon must not be empty")
	}
	if strings.TrimSpace(c.Node.TLSCertPath) == "" {
		return fmt.Errorf("node.tls_cert must not be empty")
	}
	if strings.TrimSpace(c.Node.RequiredNetwork) == "" {
		return fmt.Errorf("node.required_network must not be empty")
	}
	if strings.TrimSpace(c.Node.RequiredNetwork) != "regtest" {
		return fmt.Errorf("node.required_network must be regtest for gated writes")
	}
	if err := validateDuration("node.dial_timeout", c.Node.DialTimeout, true); err != nil {
		return err
	}
	if err := validateDuration("node.request_timeout", c.Node.RequestTimeout, true); err != nil {
		return err
	}
	if strings.TrimSpace(c.Operator.ApprovalSocket) == "" {
		return fmt.Errorf("operator.approval_socket must not be empty")
	}
	if strings.TrimSpace(c.Operator.ApprovalTokenFile) == "" {
		return fmt.Errorf("operator.approval_token_file must not be empty")
	}
	if err := validateDuration("monitor.poll_interval", c.Monitor.PollInterval, true); err != nil {
		return err
	}
	if err := validateDuration("monitor.alert_cooldown", c.Monitor.AlertCooldown, false); err != nil {
		return err
	}
	switch c.Monitor.AlertChannel {
	case "file":
		if strings.TrimSpace(c.Monitor.AlertPath) == "" {
			return fmt.Errorf("monitor.alert_path must not be empty when alert_channel is file")
		}
	case "stdout":
	default:
		return fmt.Errorf("monitor.alert_channel must be file or stdout")
	}
	return nil
}

func validateDuration(name, value string, requirePositive bool) error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s %q: %w", name, value, err)
	}
	if requirePositive && d <= 0 {
		return fmt.Errorf("%s %q must be positive", name, value)
	}
	if !requirePositive && d < 0 {
		return fmt.Errorf("%s %q must be non-negative", name, value)
	}
	return nil
}

func expandHome(path, home string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		return filepath.Join(home, path[2:])
	}
	return path
}
