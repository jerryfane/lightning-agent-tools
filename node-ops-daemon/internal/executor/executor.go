// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package executor defines the NodeExecutor interface that the daemon uses to
// perform write operations on an LND node.
//
// The concrete LND/macaroon implementation is deferred to issue #9. This
// package provides only the interface and a fail-closed stub so the rest of the
// daemon can compile without live LND dependencies.
package executor

import (
	"context"
	"errors"
)

// ErrNotImplemented is returned by the fail-closed stub until issue #9 wires
// real LND RPCs.
var ErrNotImplemented = errors.New("lnd executor not implemented")

// FeeSetRequest describes a channel policy update.
type FeeSetRequest struct {
	ChanID   uint64
	BaseMsat int64
	FeePpm   int64
}

// FeePolicy is the daemon-owned current forwarding fee policy for a channel.
type FeePolicy struct {
	BaseMsat int64
	FeePpm   int64
}

// NodeExecutor is the write-side interface to an LND node.
// Implementations must be safe for concurrent use.
type NodeExecutor interface {
	// CurrentFeePolicy returns the daemon-owned current fee policy for a
	// channel. Callers must not supply this value; it is part of the
	// enforcement state.
	CurrentFeePolicy(ctx context.Context, chanID uint64) (FeePolicy, error)

	// ExecuteFeeSet applies a new fee policy to the specified channel.
	// Returns an error if the RPC fails or the node rejects the update.
	ExecuteFeeSet(ctx context.Context, req FeeSetRequest) error
}

// StubExecutor is a fail-closed implementation used until issue #9 wires real
// RPCs. It never reports a write as successful.
type StubExecutor struct{}

// CurrentFeePolicy fails until issue #9 wires real RPCs.
func (s *StubExecutor) CurrentFeePolicy(_ context.Context, _ uint64) (FeePolicy, error) {
	return FeePolicy{}, ErrNotImplemented
}

// ExecuteFeeSet fails until issue #9 wires real RPCs.
func (s *StubExecutor) ExecuteFeeSet(_ context.Context, _ FeeSetRequest) error {
	return ErrNotImplemented
}
