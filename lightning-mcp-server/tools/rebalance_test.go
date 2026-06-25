// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// stubRebalanceClient is a minimal test double for the rebalanceClient
// interface.
type stubRebalanceClient struct {
	channels []*lnrpc.Channel
	fwdErr   bool
	fwdEvents []*lnrpc.ForwardingEvent
}

func (s *stubRebalanceClient) ListChannels(_ context.Context,
	_ *lnrpc.ListChannelsRequest,
	_ ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {
	return &lnrpc.ListChannelsResponse{Channels: s.channels}, nil
}

func (s *stubRebalanceClient) ForwardingHistory(_ context.Context,
	_ *lnrpc.ForwardingHistoryRequest,
	_ ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error) {
	if s.fwdErr {
		return nil, errors.New("unavailable")
	}
	return &lnrpc.ForwardingHistoryResponse{
		ForwardingEvents: s.fwdEvents,
	}, nil
}

// makeRebalanceReq builds a CallToolRequest with the given args.
func makeRebalanceReq(t *testing.T, args map[string]any) *mcp.CallToolRequest {
	t.Helper()
	b, err := json.Marshal(args)
	require.NoError(t, err)
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: b},
	}
}

// resultText extracts the text payload from the first content item.
func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	require.NotEmpty(t, r.Content)
	tc, ok := r.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected *mcp.TextContent")
	return tc.Text
}

// unmarshalResult unmarshals the tool result into a map.
func unmarshalResult(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, r)), &out))
	return out
}

func TestRebalanceService_ToolDefinition(t *testing.T) {
	svc := NewRebalanceService(nil)
	tool := svc.ProposeRebalanceTool()

	assert.Equal(t, "lnc_propose_rebalance", tool.Name)
	assert.Contains(t, tool.Description, "Read-only")
	assert.Contains(t, tool.Description, "rebalance")

	schema := requireToolSchema(t, tool.InputSchema)
	assert.Equal(t, "object", schema.Type)

	for _, prop := range []string{
		"imbalance_threshold",
		"max_candidates",
		"max_fee_ppm",
		"include_forwarding_demand",
	} {
		assert.Contains(t, schema.Properties, prop,
			"expected property %q in schema", prop)
	}
}

func TestRebalanceService_NilClient(t *testing.T) {
	svc := NewRebalanceService(nil)
	result, err := svc.HandleProposeRebalance(
		context.Background(), &mcp.CallToolRequest{})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestRebalanceService_BalancedChannels(t *testing.T) {
	// All channels at 50 % — no candidates expected.
	stub := &stubRebalanceClient{
		channels: []*lnrpc.Channel{
			{
				ChanId: 1, Capacity: 1_000_000,
				LocalBalance: 500_000, RemoteBalance: 500_000,
				RemotePubkey: "aaa",
			},
			{
				ChanId: 2, Capacity: 2_000_000,
				LocalBalance: 1_000_000, RemoteBalance: 1_000_000,
				RemotePubkey: "bbb",
			},
		},
	}
	svc := NewRebalanceService(stub)

	result, err := svc.HandleProposeRebalance(
		context.Background(),
		makeRebalanceReq(t, map[string]any{"include_forwarding_demand": false}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := unmarshalResult(t, result)
	candidates, ok := out["candidates"].([]any)
	require.True(t, ok)
	assert.Empty(t, candidates, "no candidates for balanced channels")
}

func TestRebalanceService_ImbalancedChannels(t *testing.T) {
	// ch1 is 80 % local (over-local), ch2 is 20 % local (over-remote).
	stub := &stubRebalanceClient{
		channels: []*lnrpc.Channel{
			{
				ChanId: 1, Capacity: 1_000_000,
				LocalBalance: 800_000, RemoteBalance: 200_000,
				RemotePubkey: "aaa",
			},
			{
				ChanId: 2, Capacity: 1_000_000,
				LocalBalance: 200_000, RemoteBalance: 800_000,
				RemotePubkey: "bbb",
			},
		},
	}
	svc := NewRebalanceService(stub)

	result, err := svc.HandleProposeRebalance(
		context.Background(),
		makeRebalanceReq(t, map[string]any{"include_forwarding_demand": false}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := unmarshalResult(t, result)
	candidates, ok := out["candidates"].([]any)
	require.True(t, ok)
	require.Len(t, candidates, 1, "expected one rebalance candidate")

	c := candidates[0].(map[string]any)
	assert.Equal(t, "1", c["out_chan"])
	assert.Equal(t, "2", c["in_chan"])
	// Both channels have 300 000 sat excess/deficit from the 500 000-sat midpoint.
	amount := int64(c["amount_sat"].(float64))
	assert.Equal(t, int64(300_000), amount)
	assert.NotEmpty(t, c["reason"])
	assert.NotEmpty(t, c["max_fee_ppm"])
}

func TestRebalanceService_MinAmountFiltered(t *testing.T) {
	// Channels are imbalanced but the rebalance amount is below the minimum.
	// Capacity 80 000 sat, 70 % local → excess = 56 000 − 40 000 = 16 000 sat
	// which is below minRebalanceAmountSat (20 000), so no candidate is proposed.
	stub := &stubRebalanceClient{
		channels: []*lnrpc.Channel{
			{
				ChanId: 1, Capacity: 80_000,
				LocalBalance: 56_000, RemoteBalance: 24_000,
				RemotePubkey: "aaa",
			},
			{
				ChanId: 2, Capacity: 80_000,
				LocalBalance: 24_000, RemoteBalance: 56_000,
				RemotePubkey: "bbb",
			},
		},
	}
	svc := NewRebalanceService(stub)

	result, err := svc.HandleProposeRebalance(
		context.Background(),
		makeRebalanceReq(t, map[string]any{"include_forwarding_demand": false}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := unmarshalResult(t, result)
	candidates := out["candidates"].([]any)
	assert.Empty(t, candidates, "amount below minimum should produce no candidates")
}

func TestRebalanceService_ForwardingDemandEnrichment(t *testing.T) {
	// ch1 receives more than it forwards (net inbound) → urgent to drain.
	stub := &stubRebalanceClient{
		channels: []*lnrpc.Channel{
			{
				ChanId: 1, Capacity: 2_000_000,
				LocalBalance: 1_600_000, RemoteBalance: 400_000,
				RemotePubkey: "aaa",
			},
			{
				ChanId: 2, Capacity: 2_000_000,
				LocalBalance: 400_000, RemoteBalance: 1_600_000,
				RemotePubkey: "bbb",
			},
		},
		fwdEvents: []*lnrpc.ForwardingEvent{
			// More in than out on channel 1 → negative net flow.
			{ChanIdIn: 1, AmtIn: 500_000, ChanIdOut: 99, AmtOut: 0},
		},
	}
	svc := NewRebalanceService(stub)

	result, err := svc.HandleProposeRebalance(
		context.Background(),
		makeRebalanceReq(t, map[string]any{"include_forwarding_demand": true}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := unmarshalResult(t, result)
	candidates := out["candidates"].([]any)
	require.Len(t, candidates, 1)
	reason := candidates[0].(map[string]any)["reason"].(string)
	assert.Contains(t, reason, "urgent", "demand pressure should mark candidate as urgent")
}

func TestRebalanceService_ForwardingHistoryError(t *testing.T) {
	// A ForwardingHistory failure must not break the response.
	stub := &stubRebalanceClient{
		channels: []*lnrpc.Channel{
			{
				ChanId: 1, Capacity: 1_000_000,
				LocalBalance: 800_000, RemoteBalance: 200_000,
			},
			{
				ChanId: 2, Capacity: 1_000_000,
				LocalBalance: 200_000, RemoteBalance: 800_000,
			},
		},
		fwdErr: true,
	}
	svc := NewRebalanceService(stub)

	result, err := svc.HandleProposeRebalance(
		context.Background(),
		makeRebalanceReq(t, map[string]any{"include_forwarding_demand": true}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError, "forwarding history error should degrade gracefully")

	out := unmarshalResult(t, result)
	candidates := out["candidates"].([]any)
	require.Len(t, candidates, 1, "candidate still returned despite forwarding error")
}

func TestRebalanceService_MaxCandidatesRespected(t *testing.T) {
	channels := make([]*lnrpc.Channel, 0, 6)
	for i := uint64(1); i <= 3; i++ {
		channels = append(channels, &lnrpc.Channel{
			ChanId: i, Capacity: 1_000_000,
			LocalBalance: 800_000, RemoteBalance: 200_000,
		})
	}
	for i := uint64(4); i <= 6; i++ {
		channels = append(channels, &lnrpc.Channel{
			ChanId: i, Capacity: 1_000_000,
			LocalBalance: 200_000, RemoteBalance: 800_000,
		})
	}
	stub := &stubRebalanceClient{channels: channels}
	svc := NewRebalanceService(stub)

	result, err := svc.HandleProposeRebalance(
		context.Background(),
		makeRebalanceReq(t, map[string]any{
			"max_candidates":           2,
			"include_forwarding_demand": false,
		}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := unmarshalResult(t, result)
	candidates := out["candidates"].([]any)
	assert.LessOrEqual(t, len(candidates), 2,
		"max_candidates must be respected")
}

func TestRebalanceService_AnalysisFields(t *testing.T) {
	stub := &stubRebalanceClient{
		channels: []*lnrpc.Channel{
			{ChanId: 1, Capacity: 1_000_000, LocalBalance: 800_000, RemoteBalance: 200_000},
			{ChanId: 2, Capacity: 1_000_000, LocalBalance: 200_000, RemoteBalance: 800_000},
			{ChanId: 3, Capacity: 1_000_000, LocalBalance: 500_000, RemoteBalance: 500_000},
		},
	}
	svc := NewRebalanceService(stub)

	result, err := svc.HandleProposeRebalance(
		context.Background(),
		makeRebalanceReq(t, map[string]any{"include_forwarding_demand": false}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := unmarshalResult(t, result)
	analysis, ok := out["analysis"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, float64(3), analysis["total_active_channels"])
	overLocal := analysis["over_local_channels"].([]any)
	overRemote := analysis["over_remote_channels"].([]any)
	assert.Len(t, overLocal, 1)
	assert.Len(t, overRemote, 1)

	// Verify per-channel map fields.
	ch := overLocal[0].(map[string]any)
	assert.Contains(t, ch, "chan_id")
	assert.Contains(t, ch, "local_balance")
	assert.Contains(t, ch, "local_ratio")
}
