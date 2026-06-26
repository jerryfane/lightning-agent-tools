// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// mockFeeClient satisfies lnrpc.LightningClient for the two methods
// HandleProposeFees calls; all other methods panic if called.
type mockFeeClient struct {
	lnrpc.LightningClient
	forwardingHistory func(context.Context, *lnrpc.ForwardingHistoryRequest, ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error)
	listChannels      func(context.Context, *lnrpc.ListChannelsRequest, ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error)
	feeReport         func(context.Context, *lnrpc.FeeReportRequest, ...grpc.CallOption) (*lnrpc.FeeReportResponse, error)
}

func (m *mockFeeClient) ForwardingHistory(ctx context.Context, in *lnrpc.ForwardingHistoryRequest, opts ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error) {
	return m.forwardingHistory(ctx, in, opts...)
}

func (m *mockFeeClient) ListChannels(ctx context.Context, in *lnrpc.ListChannelsRequest, opts ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {
	return m.listChannels(ctx, in, opts...)
}

func (m *mockFeeClient) FeeReport(ctx context.Context, in *lnrpc.FeeReportRequest, opts ...grpc.CallOption) (*lnrpc.FeeReportResponse, error) {
	if m.feeReport == nil {
		return &lnrpc.FeeReportResponse{}, nil
	}
	return m.feeReport(ctx, in, opts...)
}

// makeFeeRequest builds a CallToolRequest with the given JSON arguments.
func makeFeeRequest(args map[string]any) *mcp.CallToolRequest {
	b, _ := json.Marshal(args)
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Arguments: json.RawMessage(b),
		},
	}
}

// emptyChannelsMock returns a listChannels stub that always returns zero
// channels — useful when the test only cares about history behaviour.
func emptyChannelsMock() func(context.Context, *lnrpc.ListChannelsRequest, ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {
	return func(_ context.Context, _ *lnrpc.ListChannelsRequest, _ ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {
		return &lnrpc.ListChannelsResponse{}, nil
	}
}

// --- proposeFee unit tests (bug-4 regression + full branch coverage) ---

func TestProposeFee_DepletedUsesRangeRelativeFormula(t *testing.T) {
	// Bug 4: old code used maxPPM*0.80 (absolute), new code must use the
	// range-relative pattern minPPM + (maxPPM-minPPM)*0.90.
	min, max := 10.0, 1000.0
	got := proposeFee(0.1, 5, min, max) // localRatio < 0.2 → depleted
	want := int64(min + (max-min)*0.90) // 10 + 990*0.90 = 901
	assert.Equal(t, want, got,
		"depleted branch must use range-relative formula")

	// Demonstrate that the old formula gives a different (wrong) answer when
	// minPPM != 0.
	oldFormula := int64(max * 0.80) // 800
	assert.NotEqual(t, oldFormula, got,
		"result must differ from the old absolute-percentage formula")
}

func TestProposeFee_AllBranches(t *testing.T) {
	min, max := 10.0, 1000.0
	range_ := max - min // 990

	tests := []struct {
		name       string
		localRatio float64
		forwards   int64
		want       int64
	}{
		{
			name:       "depleted",
			localRatio: 0.1,
			forwards:   3,
			want:       int64(min + range_*0.90), // 901
		},
		{
			name:       "saturated",
			localRatio: 0.9,
			forwards:   3,
			want:       int64(min + range_*0.10), // 109
		},
		{
			name:       "idle_balanced",
			localRatio: 0.5,
			forwards:   0,
			want:       int64(min + range_*0.15), // 158
		},
		{
			name:       "balanced_active_mid",
			localRatio: 0.5,
			forwards:   10,
			want:       int64(min + range_*(1-0.5)), // 505
		},
		{
			name:       "balanced_active_low_local",
			localRatio: 0.3,
			forwards:   10,
			want:       int64(min + range_*(1-0.3)), // 703
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := proposeFee(tt.localRatio, tt.forwards, min, max)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestProposeFee_Clamping(t *testing.T) {
	// When min == max every branch should return min (no division by zero).
	got := proposeFee(0.1, 0, 100, 100)
	assert.Equal(t, int64(100), got)

	// Result is always within [min, max].
	got = proposeFee(0.0, 0, 1, 5000)
	assert.GreaterOrEqual(t, got, int64(1))
	assert.LessOrEqual(t, got, int64(5000))
}

// --- FeeService tool-creation tests ---

func TestFeeService_ProposeFeesTool(t *testing.T) {
	svc := NewFeeService(nil)
	tool := svc.ProposeFeesTool()

	assert.Equal(t, "lnc_propose_fees", tool.Name)
	assert.NotEmpty(t, tool.Description)

	schema := requireToolSchema(t, tool.InputSchema)
	assert.Equal(t, "object", schema.Type)
	assert.Contains(t, schema.Properties, "days")
	assert.Contains(t, schema.Properties, "min_fee_ppm")
	assert.Contains(t, schema.Properties, "max_fee_ppm")

	days, ok := schema.Properties["days"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, 0, days["exclusiveMinimum"])
	assert.NotContains(t, days, "minimum")
}

// --- HandleProposeFees handler tests ---

// TestHandleProposeFees_NoClient verifies the "not connected" guard.
func TestHandleProposeFees_NoClient(t *testing.T) {
	svc := NewFeeService(nil)
	result, err := svc.HandleProposeFees(context.Background(), makeFeeRequest(nil))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

// TestHandleProposeFees_InvertedMinMax covers bug-2: inverted bounds must
// be rejected before any API call is made.
func TestHandleProposeFees_InvertedMinMax(t *testing.T) {
	called := false
	mock := &mockFeeClient{
		forwardingHistory: func(_ context.Context, _ *lnrpc.ForwardingHistoryRequest, _ ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error) {
			called = true
			return &lnrpc.ForwardingHistoryResponse{}, nil
		},
		listChannels: emptyChannelsMock(),
	}

	svc := NewFeeService(mock)
	result, err := svc.HandleProposeFees(context.Background(), makeFeeRequest(map[string]any{
		"min_fee_ppm": 5000,
		"max_fee_ppm": 1,
	}))
	require.NoError(t, err)
	assert.True(t, result.IsError, "inverted bounds must produce an error result")
	assert.False(t, called, "no API call must be made when bounds are inverted")
}

// TestHandleProposeFees_FractionalDays covers bug-1: a fractional day value
// must produce a startTime that is proportionally in the past, not zero.
func TestHandleProposeFees_FractionalDays(t *testing.T) {
	var capturedStart uint64
	mock := &mockFeeClient{
		forwardingHistory: func(_ context.Context, in *lnrpc.ForwardingHistoryRequest, _ ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error) {
			capturedStart = in.StartTime
			return &lnrpc.ForwardingHistoryResponse{}, nil
		},
		listChannels: emptyChannelsMock(),
	}

	before := time.Now()
	svc := NewFeeService(mock)
	result, err := svc.HandleProposeFees(context.Background(), makeFeeRequest(map[string]any{
		"days": 0.5,
	}))
	require.NoError(t, err)
	require.False(t, result.IsError)

	// 0.5 days = 12 hours ago.  Allow ±5 s for test execution.
	expected := uint64(before.Add(-12 * time.Hour).Unix())
	delta := int64(capturedStart) - int64(expected)
	if delta < 0 {
		delta = -delta
	}
	assert.LessOrEqual(t, delta, int64(5),
		"startTime for 0.5 days must be ~12 hours ago, not zero")

	text := result.Content[0].(*mcp.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	assert.Equal(t, 0.5, resp["lookback_days"])
}

func TestHandleProposeFees_IncludesCurrentFees(t *testing.T) {
	mock := &mockFeeClient{
		forwardingHistory: func(_ context.Context, _ *lnrpc.ForwardingHistoryRequest, _ ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error) {
			return &lnrpc.ForwardingHistoryResponse{}, nil
		},
		listChannels: func(_ context.Context, _ *lnrpc.ListChannelsRequest, _ ...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {
			return &lnrpc.ListChannelsResponse{
				Channels: []*lnrpc.Channel{{
					ChanId: 1, Capacity: 1_000_000,
					LocalBalance: 500_000, RemoteBalance: 500_000,
				}},
			}, nil
		},
		feeReport: func(_ context.Context, _ *lnrpc.FeeReportRequest, _ ...grpc.CallOption) (*lnrpc.FeeReportResponse, error) {
			return &lnrpc.FeeReportResponse{
				ChannelFees: []*lnrpc.ChannelFeeReport{{
					ChanId:       1,
					BaseFeeMsat:  1_000,
					FeePerMil:    250,
					ChannelPoint: "tx:0",
				}},
			}, nil
		},
	}

	svc := NewFeeService(mock)
	result, err := svc.HandleProposeFees(context.Background(), makeFeeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)

	text := result.Content[0].(*mcp.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	proposals := resp["proposals"].([]any)
	require.Len(t, proposals, 1)
	proposal := proposals[0].(map[string]any)

	assert.Equal(t, float64(250), proposal["current_fee_ppm"])
	assert.Equal(t, float64(1_000), proposal["current_base_fee_msat"])
	assert.Equal(t,
		proposal["proposed_fee_ppm"].(float64)-proposal["current_fee_ppm"].(float64),
		proposal["fee_delta_ppm"],
	)
}

// TestHandleProposeFees_Pagination covers bug-3: history spanning more than
// 50 000 events must be fully fetched via multiple pages.
func TestHandleProposeFees_Pagination(t *testing.T) {
	const firstPageSize = int(feePageSize) // 50 000 events on first page
	callCount := 0

	mock := &mockFeeClient{
		forwardingHistory: func(_ context.Context, in *lnrpc.ForwardingHistoryRequest, _ ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error) {
			callCount++
			if callCount == 1 {
				require.Equal(t, uint32(0), in.IndexOffset)
				// First page: return a full page of events.
				events := make([]*lnrpc.ForwardingEvent, firstPageSize)
				for i := range events {
					events[i] = &lnrpc.ForwardingEvent{ChanIdOut: 1}
				}
				return &lnrpc.ForwardingHistoryResponse{
					ForwardingEvents: events,
					LastOffsetIndex:  uint32(firstPageSize),
				}, nil
			}

			require.Equal(t, uint32(firstPageSize), in.IndexOffset)
			// Second page: return one more event to complete pagination.
			return &lnrpc.ForwardingHistoryResponse{
				ForwardingEvents: []*lnrpc.ForwardingEvent{
					{ChanIdOut: 1},
				},
				LastOffsetIndex: uint32(firstPageSize),
			}, nil
		},
		listChannels: emptyChannelsMock(),
	}

	svc := NewFeeService(mock)
	result, err := svc.HandleProposeFees(context.Background(), makeFeeRequest(nil))
	require.NoError(t, err)
	require.False(t, result.IsError)

	// Decode response and verify total_forwards includes both pages.
	text := result.Content[0].(*mcp.TextContent).Text
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &resp))

	totalForwards, ok := resp["total_forwards"].(float64)
	require.True(t, ok)
	assert.Equal(t, float64(firstPageSize+1), totalForwards,
		"all events across pages must be counted")
	assert.Equal(t, 2, callCount, "must make exactly two ForwardingHistory calls")
}
