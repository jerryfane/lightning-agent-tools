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

	// MaxFeePpmDelta is the largest single fee change (absolute, in ppm) the
	// daemon may auto-execute.
	MaxFeePpmDelta int64 `toml:"max_fee_ppm_delta"`

	// PerChannelCooldown is a Go duration string (e.g. "1h") enforcing a
	// minimum interval between successive operations on the same channel.
	PerChannelCooldown string `toml:"per_channel_cooldown"`

	// RebalanceMaxFeePpm is the ceiling fee rate (ppm) for any rebalance route.
	RebalanceMaxFeePpm int64 `toml:"rebalance_max_fee_ppm"`
}

// Approval decides whether requests are auto-executed or queued.
type Approval struct {
	// AutoExecuteBelowPpmDelta: if |delta| <= this value and RequireApproval
	// is false, the daemon executes immediately without human review.
	AutoExecuteBelowPpmDelta int64 `toml:"auto_execute_below_ppm_delta"`

	// RequireApproval forces every action into the pending queue regardless
	// of size.
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
