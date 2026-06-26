// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

	// KillswitchFile is the path whose mere presence halts all execution.
	KillswitchFile string `toml:"killswitch"`
}

// Config is the root configuration for the daemon.
type Config struct {
	Limits   Limits   `toml:"limits"`
	Approval Approval `toml:"approval"`
	Storage  Storage  `toml:"storage"`
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
			LedgerPath:     filepath.Join(base, "ledger.db"),
			KillswitchFile: filepath.Join(base, "STOP"),
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
	c.Storage.KillswitchFile = expandHome(c.Storage.KillswitchFile, home)
	return nil
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Storage.LedgerPath) == "" {
		return fmt.Errorf("storage.ledger must not be empty")
	}
	if strings.TrimSpace(c.Storage.KillswitchFile) == "" {
		return fmt.Errorf("storage.killswitch must not be empty")
	}
	return nil
}

func expandHome(path, home string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		return filepath.Join(home, path[2:])
	}
	return path
}
