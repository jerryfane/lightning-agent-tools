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

type mockChannelActionsClient struct {
	lnrpc.LightningClient
	channels     []*lnrpc.Channel
	fwdResponses []*lnrpc.ForwardingHistoryResponse
	fwdRequests  []*lnrpc.ForwardingHistoryRequest
	info         *lnrpc.GetInfoResponse
	graph        *lnrpc.ChannelGraph
	graphErr     error
}

func (m *mockChannelActionsClient) ListChannels(context.Context,
	*lnrpc.ListChannelsRequest,
	...grpc.CallOption) (*lnrpc.ListChannelsResponse, error) {

	return &lnrpc.ListChannelsResponse{Channels: m.channels}, nil
}

func (m *mockChannelActionsClient) GetInfo(context.Context,
	*lnrpc.GetInfoRequest,
	...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	if m.info != nil {
		return m.info, nil
	}
	return &lnrpc.GetInfoResponse{IdentityPubkey: "local"}, nil
}

func (m *mockChannelActionsClient) ForwardingHistory(_ context.Context,
	req *lnrpc.ForwardingHistoryRequest,
	_ ...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error) {

	m.fwdRequests = append(m.fwdRequests, &lnrpc.ForwardingHistoryRequest{
		StartTime:    req.StartTime,
		EndTime:      req.EndTime,
		NumMaxEvents: req.NumMaxEvents,
		IndexOffset:  req.IndexOffset,
	})
	idx := len(m.fwdRequests) - 1
	if idx < len(m.fwdResponses) {
		return m.fwdResponses[idx], nil
	}
	return &lnrpc.ForwardingHistoryResponse{}, nil
}

func (m *mockChannelActionsClient) DescribeGraph(context.Context,
	*lnrpc.ChannelGraphRequest,
	...grpc.CallOption) (*lnrpc.ChannelGraph, error) {

	if m.graphErr != nil {
		return nil, m.graphErr
	}
	if m.graph != nil {
		return m.graph, nil
	}
	return &lnrpc.ChannelGraph{}, nil
}

func makeChannelActionsReq(t *testing.T,
	args map[string]any) *mcp.CallToolRequest {

	t.Helper()
	b, err := json.Marshal(args)
	require.NoError(t, err)
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Arguments: b},
	}
}

func TestChannelActionsService_ForwardingHistoryPagination(t *testing.T) {
	firstPage := make(
		[]*lnrpc.ForwardingEvent, channelActionsHistoryPageSize,
	)
	for i := range firstPage {
		firstPage[i] = &lnrpc.ForwardingEvent{ChanIdIn: 99}
	}
	stub := &mockChannelActionsClient{
		channels: []*lnrpc.Channel{{
			ChanId: 2, Active: true, NumUpdates: 1,
			TotalSatoshisSent: 1, RemotePubkey: "peer",
		}},
		fwdResponses: []*lnrpc.ForwardingHistoryResponse{
			{
				ForwardingEvents: firstPage,
				LastOffsetIndex:  channelActionsHistoryPageSize,
			},
			{
				ForwardingEvents: []*lnrpc.ForwardingEvent{{
					ChanIdOut: 2,
				}},
				LastOffsetIndex: channelActionsHistoryPageSize,
			},
		},
	}
	svc := NewChannelActionsService(stub)

	result, err := svc.HandleProposeChannelActions(
		context.Background(), makeChannelActionsReq(t, nil),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	require.Len(t, stub.fwdRequests, 2)
	assert.Equal(t, uint32(0), stub.fwdRequests[0].IndexOffset)
	assert.Equal(t, uint32(channelActionsHistoryPageSize),
		stub.fwdRequests[1].IndexOffset)

	out := unmarshalResult(t, result)
	assert.Equal(t, float64(channelActionsHistoryPageSize+1),
		out["forwarding_events"])
	assert.Empty(t, out["close_candidates"].([]any))
}

func TestChannelActionsService_DescribeGraphErrorKeepsCloseCandidates(
	t *testing.T) {

	stub := &mockChannelActionsClient{
		channels: []*lnrpc.Channel{{
			ChanId: 1, Active: false, RemotePubkey: "peer",
		}},
		graphErr: errors.New("graph unavailable"),
	}
	svc := NewChannelActionsService(stub)

	result, err := svc.HandleProposeChannelActions(
		context.Background(), makeChannelActionsReq(t, map[string]any{
			"lookback_days":        999,
			"max_close_candidates": 999,
			"max_open_candidates":  999,
		}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := unmarshalResult(t, result)
	assert.Equal(t, float64(maxLookbackDays),
		out["analysis_window_days"])
	assert.Contains(t, out["open_candidates_error"], "graph unavailable")
	assert.Empty(t, out["open_candidates"].([]any))
	require.Len(t, out["close_candidates"].([]any), 1)
}

func TestChannelActionsService_OpenCandidatesExcludeLocalNode(t *testing.T) {
	stub := &mockChannelActionsClient{
		info: &lnrpc.GetInfoResponse{IdentityPubkey: "local"},
		graph: &lnrpc.ChannelGraph{
			Nodes: []*lnrpc.LightningNode{
				{PubKey: "local", Alias: "local-node"},
				{PubKey: "candidate", Alias: "candidate-node"},
			},
			Edges: []*lnrpc.ChannelEdge{
				{Node1Pub: "local", Node2Pub: "a", Capacity: 1},
				{Node1Pub: "local", Node2Pub: "b", Capacity: 1},
				{Node1Pub: "local", Node2Pub: "c", Capacity: 1},
				{Node1Pub: "local", Node2Pub: "d", Capacity: 1},
				{Node1Pub: "local", Node2Pub: "e", Capacity: 1},
				{Node1Pub: "candidate", Node2Pub: "a", Capacity: 2},
				{Node1Pub: "candidate", Node2Pub: "b", Capacity: 2},
				{Node1Pub: "candidate", Node2Pub: "c", Capacity: 2},
				{Node1Pub: "candidate", Node2Pub: "d", Capacity: 2},
				{Node1Pub: "candidate", Node2Pub: "e", Capacity: 2},
			},
		},
	}
	svc := NewChannelActionsService(stub)

	result, err := svc.HandleProposeChannelActions(
		context.Background(), makeChannelActionsReq(t, nil),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := unmarshalResult(t, result)
	candidates := out["open_candidates"].([]any)
	require.Len(t, candidates, 1)
	candidate := candidates[0].(map[string]any)
	assert.Equal(t, "candidate", candidate["peer_pubkey"])
}
