// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package main implements the MCP LNC server daemon. It exposes Lightning
// Network Daemon (LND) nodes through the Model Context Protocol (MCP) using
// Lightning Node Connect (LNC), enabling AI assistants to securely query
// Lightning Network data over WebSocket tunnels and submit daemon-gated local
// node-ops requests.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lightninglabs/lightning-agent-kit/lightning-mcp-server/internal/config"
	lnccontext "github.com/lightninglabs/lightning-agent-kit/lightning-mcp-server/internal/context"
	"github.com/lightninglabs/lightning-agent-kit/lightning-mcp-server/internal/logging"
	"go.uber.org/zap"
)

// Daemon coordinates the MCP server lifecycle and shutdown orchestration.
type Daemon struct {
	cfg    *config.Config
	logger *zap.Logger
	server *Server

	// quit is used to signal shutdown.
	quit chan struct{}

	// shutdownComplete is closed when all shutdown operations are complete.
	shutdownComplete chan struct{}
}

// NewDaemon constructs a daemon instance with the provided configuration.
func NewDaemon(cfg *config.Config, logger *zap.Logger) (*Daemon, error) {
	server, err := NewServer(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	return &Daemon{
		cfg:              cfg,
		logger:           logger,
		server:           server,
		quit:             make(chan struct{}),
		shutdownComplete: make(chan struct{}),
	}, nil
}

// Start runs the daemon until a shutdown signal or server failure occurs.
func (d *Daemon) Start() error {
	// Create context for daemon startup.
	ctx := lnccontext.New(context.Background(), "daemon_start", 0)
	defer ctx.Cancel()
	logger := logging.LogWithContext(ctx)

	logger.Info("Starting MCP LNC Server daemon",
		zap.String("version", d.cfg.ServerVersion),
		zap.Bool("development", d.cfg.Development),
	)

	// Start the server in a goroutine.
	serverErrChan := make(chan error, 1)
	go func() {
		if err := d.server.Start(); err != nil {
			serverErrChan <- err
		}
	}()

	// Set up signal handling for graceful shutdown.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start shutdown handler.
	go d.shutdownHandler()

	// Wait for either a shutdown signal or server error.
	select {
	case sig := <-sigChan:
		logger.Info("Received shutdown signal",
			zap.String("signal", sig.String()),
			zap.Duration("uptime", ctx.Duration()))
		close(d.quit)

	case err := <-serverErrChan:
		if err != nil && err != context.Canceled {
			logger.Error("Server error",
				zap.Error(err),
				zap.Duration("uptime", ctx.Duration()))
			close(d.quit)
			return err
		}

	case <-d.quit:
		// Shutdown was triggered internally
	}

	// Wait for shutdown to complete.
	<-d.shutdownComplete
	logger.Info("MCP LNC Server daemon shutdown complete",
		zap.Duration("total_uptime", ctx.Duration()))

	return nil
}

// Stop triggers a graceful shutdown of the daemon.
func (d *Daemon) Stop() {
	select {
	case <-d.quit:
		// Already shutting down.
		return
	default:
	}

	ctx := lnccontext.New(context.Background(), "daemon_stop",
		5*time.Second)
	defer ctx.Cancel()
	logger := logging.LogWithContext(ctx)
	logger.Info("Initiating daemon shutdown...")
	close(d.quit)
}

// shutdownHandler drains the quit channel and coordinates shutdown.
func (d *Daemon) shutdownHandler() {
	<-d.quit

	// Create context for shutdown with timeout.
	shutdownCtx := lnccontext.New(
		context.Background(),
		"daemon_shutdown",
		d.cfg.ShutdownTimeout,
	)
	defer shutdownCtx.Cancel()
	logger := logging.LogWithContext(shutdownCtx)

	logger.Info("Beginning graceful shutdown...",
		zap.Duration("timeout", d.cfg.ShutdownTimeout))

	// Stop the server.
	if err := d.server.Stop(shutdownCtx); err != nil {
		logger.Error("Error during server shutdown",
			zap.Error(err),
			zap.Duration("shutdown_duration", shutdownCtx.Duration()))
	} else {
		logger.Info("Server shutdown completed successfully",
			zap.Duration("shutdown_duration", shutdownCtx.Duration()))
	}

	// Signal shutdown complete.
	close(d.shutdownComplete)
}

// main is the entry point for the MCP LNC server daemon.
func main() {
	// Parse command line flags
	var version = flag.Bool("version", false, "Show version information")
	flag.Parse()

	// Load configuration
	cfg := config.LoadConfig()

	// Handle version flag
	if *version {
		fmt.Printf("MCP LNC Server %s\n", cfg.ServerVersion)
		fmt.Println("Lightning Network integration for AI assistants")
		fmt.Println("https://github.com/lightninglabs/lightning-agent-kit")
		os.Exit(0)
	}

	// Initialize logging
	if err := logging.InitLogger(cfg.Development); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logging.Sync()

	logger := logging.Logger

	// Create and start the daemon
	daemon, err := NewDaemon(cfg, logger)
	if err != nil {
		logger.Error("Failed to create daemon", zap.Error(err))
		os.Exit(1)
	}

	if err := daemon.Start(); err != nil {
		logger.Error("Daemon startup failed", zap.Error(err))
		os.Exit(1)
	}
}
