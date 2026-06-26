// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// node-ops-daemon is a long-lived daemon that governs LND node operations with
// configurable limits, a human-approval queue, an append-only audit ledger, and
// a file-presence kill-switch.
//
// Usage:
//
//	node-ops-daemon [config-path]
//
// If config-path is omitted, defaults to ~/.node-ops/config.toml.
// The daemon listens on ~/.node-ops/daemon.sock (mode 0600).
//
// Wire protocol: each message is a 4-byte big-endian uint32 length followed by
// a JSON body. Requests: {"action":"...","params":{...}}. Responses:
// {"status":"ok|error|pending","request_id":"...","result":...,"reason":"..."}.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/daemon"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/executor"
)

func main() {
	cfgPath := config.DefaultPath()
	explicitConfig := len(os.Args) > 1
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		if explicitConfig || !os.IsNotExist(err) {
			log.Fatalf("load config: %v", err)
		}
		log.Printf("warn: config %s not found; using built-in defaults", cfgPath)
		cfg = config.Defaults()
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot determine home directory: %v", err)
	}
	sockPath := filepath.Join(home, ".node-ops", "daemon.sock")

	d, err := daemon.New(cfg, &executor.StubExecutor{})
	if err != nil {
		log.Fatalf("init daemon: %v", err)
	}
	defer d.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("node-ops-daemon: socket %s  ledger %s\n", sockPath, cfg.Storage.LedgerPath)
	if err := d.Run(ctx, sockPath); err != nil {
		log.Fatalf("daemon: %v", err)
	}
}
