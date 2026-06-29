// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package executor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/executor"
)

func TestStubExecutorFailsClosed(t *testing.T) {
	stub := &executor.StubExecutor{}
	if _, err := stub.CurrentFeePolicy(context.Background(), 1); !errors.Is(err, executor.ErrNotImplemented) {
		t.Fatalf("CurrentFeePolicy error = %v, want ErrNotImplemented", err)
	}
	err := stub.ExecuteFeeSet(context.Background(), executor.FeeSetRequest{
		ChanID: 1, BaseMsat: 1000, FeePpm: 100,
	})
	if !errors.Is(err, executor.ErrNotImplemented) {
		t.Fatalf("ExecuteFeeSet error = %v, want ErrNotImplemented", err)
	}
	if _, err := stub.ExecuteRebalance(context.Background(), executor.RebalanceRequest{
		OutgoingChanID: 1,
		IncomingChanID: 2,
		AmountSat:      100,
		MaxFeePpm:      50,
	}); !errors.Is(err, executor.ErrNotImplemented) {
		t.Fatalf("ExecuteRebalance error = %v, want ErrNotImplemented", err)
	}
	if _, err := stub.NodeHealth(context.Background()); !errors.Is(err, executor.ErrNotImplemented) {
		t.Fatalf("NodeHealth error = %v, want ErrNotImplemented", err)
	}
}
