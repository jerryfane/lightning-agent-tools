// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package config provides configuration management helpers for the MCP LNC server.
package config

import (
	"os"
	"strconv"
	"time"
)

// Config captures all runtime configuration for the MCP LNC server.
type Config struct {
	// Server configuration.
	ServerName    string
	ServerVersion string
	Development   bool

	// LNC connection defaults.
	DefaultMailboxServer string
	DefaultTimeout       time.Duration
	DefaultDevMode       bool
	DefaultInsecure      bool

	// Security settings.
	MaxConnectionRetries int
	ConnectionTimeout    time.Duration
	ShutdownTimeout      time.Duration
}

// LoadConfig populates Config from environment variables with sensible defaults.
func LoadConfig() *Config {
	cfg := &Config{
		// Server defaults.
		ServerName:    "lightning-mcp-server",
		ServerVersion: "1.0.0",
		Development:   getEnvBool("DEVELOPMENT", false),

		// LNC defaults.
		DefaultMailboxServer: getEnvString("LNC_DEFAULT_MAILBOX",
			"mailbox.terminal.lightning.today:443"),
		DefaultTimeout: getEnvDuration("LNC_DEFAULT_TIMEOUT",
			30*time.Second),
		DefaultDevMode:  getEnvBool("LNC_DEFAULT_DEV_MODE", false),
		DefaultInsecure: getEnvBool("LNC_DEFAULT_INSECURE", false),

		// Security defaults.
		MaxConnectionRetries: getEnvInt("LNC_MAX_RETRIES", 3),
		ConnectionTimeout: getEnvDuration("LNC_CONNECTION_TIMEOUT",
			30*time.Second),
		ShutdownTimeout: getEnvDuration("SHUTDOWN_TIMEOUT",
			30*time.Second),
	}

	return cfg
}

// getEnvString retrieves a string value from environment variables with a fallback.
func getEnvString(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvInt retrieves an integer value from environment variables with a fallback.
func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

// getEnvBool retrieves a boolean value from environment variables with a fallback.
func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}

// getEnvDuration retrieves a duration value from environment variables with a fallback.
func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}
