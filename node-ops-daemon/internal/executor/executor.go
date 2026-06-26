// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package executor defines the NodeExecutor interface that the daemon uses to
// perform write operations on an LND node.
//
// The concrete LND/macaroon implementation is deferred to issue #9. This
// package provides only the interface and a no-op stub so the rest of the
// daemon can compile and be tested without live LND dependencies.
package executor

import "context"

// FeeSetRequest describes a channel policy update.
type FeeSetRequest struct {
	ChanID   uint64
	BaseMsat int64
	FeePpm   int64
}

// NodeExecutor is the write-side interface to an LND node.
// Implementations must be safe for concurrent use.
type NodeExecutor interface {
	// CurrentFeePpm returns the daemon-owned current fee rate for a channel.
	// Callers must not supply this value; it is part of the enforcement state.
	CurrentFeePpm(ctx context.Context, chanID uint64) (int64, error)

	// ExecuteFeeSet applies a new fee policy to the specified channel.
	// Returns an error if the RPC fails or the node rejects the update.
	ExecuteFeeSet(ctx context.Context, req FeeSetRequest) error
}

// StubExecutor is a no-op implementation used until issue #9 wires real RPCs.
// It succeeds silently for all requests.
type StubExecutor struct{}

// CurrentFeePpm returns a stable stub value until issue #9 wires real RPCs.
func (s *StubExecutor) CurrentFeePpm(_ context.Context, _ uint64) (int64, error) {
	return 0, nil
}

// ExecuteFeeSet is a stub that always succeeds without contacting LND.
func (s *StubExecutor) ExecuteFeeSet(_ context.Context, _ FeeSetRequest) error {
	return nil
}
