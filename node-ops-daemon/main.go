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
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

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
		if explicitConfig || !errors.Is(err, os.ErrNotExist) {
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

	nodeExec, err := newNodeExecutor(context.Background(), cfg)
	if err != nil {
		log.Printf("warn: concrete LND executor unavailable; "+
			"writes will fail closed: %v", err)
		nodeExec = &executor.StubExecutor{}
	}

	d, err := daemon.New(cfg, nodeExec)
	if err != nil {
		log.Fatalf("init daemon: %v", err)
	}
	defer d.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("node-ops-daemon: socket %s  operator %s  ledger %s\n",
		sockPath, cfg.Operator.ApprovalSocket, cfg.Storage.LedgerPath)
	if err := d.RunWithOperator(ctx, sockPath, cfg.Operator.ApprovalSocket); err != nil {
		log.Fatalf("daemon: %v", err)
	}
}

func newNodeExecutor(ctx context.Context,
	cfg *config.Config) (executor.NodeExecutor, error) {

	dialTimeout, err := time.ParseDuration(cfg.Node.DialTimeout)
	if err != nil {
		return nil, err
	}
	requestTimeout, err := time.ParseDuration(cfg.Node.RequestTimeout)
	if err != nil {
		return nil, err
	}
	return executor.NewLNDExecutor(ctx, executor.LNDConfig{
		RPCAddress:      cfg.Node.LndRPC,
		MacaroonPath:    cfg.Node.MacaroonPath,
		TLSCertPath:     cfg.Node.TLSCertPath,
		RequiredNetwork: cfg.Node.RequiredNetwork,
		DialTimeout:     dialTimeout,
		RequestTimeout:  requestTimeout,
	})
}
