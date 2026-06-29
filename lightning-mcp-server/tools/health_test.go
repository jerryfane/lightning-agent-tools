// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type mockHealthClient struct {
	lnrpc.LightningClient
	info    *lnrpc.GetInfoResponse
	pending *lnrpc.PendingChannelsResponse
	peers   *lnrpc.ListPeersResponse
}

func (m *mockHealthClient) GetInfo(context.Context, *lnrpc.GetInfoRequest,
	...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	return m.info, nil
}

func (m *mockHealthClient) PendingChannels(context.Context,
	*lnrpc.PendingChannelsRequest,
	...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {

	return m.pending, nil
}

func (m *mockHealthClient) ListPeers(context.Context, *lnrpc.ListPeersRequest,
	...grpc.CallOption) (*lnrpc.ListPeersResponse, error) {

	return m.peers, nil
}

func decodeHealthResult(t *testing.T,
	result *mcp.CallToolResult) map[string]any {

	t.Helper()
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text.Text), &out))
	return out
}

func TestHealthService_HealthyAlertsAreEmptyArray(t *testing.T) {
	svc := NewHealthService(&mockHealthClient{
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: "node",
			Alias:          "test-node",
			SyncedToChain:  true,
			SyncedToGraph:  true,
			BlockHeight:    42,
		},
		pending: &lnrpc.PendingChannelsResponse{},
		peers:   &lnrpc.ListPeersResponse{},
	})

	result, err := svc.HandleNodeHealth(
		context.Background(), &mcp.CallToolRequest{},
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := decodeHealthResult(t, result)
	assert.Equal(t, "healthy", out["overall_status"])
	assert.Equal(t, float64(0), out["alert_count"])
	alerts, ok := out["alerts"].([]any)
	require.True(t, ok)
	assert.Empty(t, alerts)
}

func TestHealthService_CriticalAlertsArePrioritized(t *testing.T) {
	svc := NewHealthService(&mockHealthClient{
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: "node",
			Alias:          "test-node",
			SyncedToChain:  true,
			SyncedToGraph:  false,
			BlockHeight:    42,
		},
		pending: &lnrpc.PendingChannelsResponse{
			PendingForceClosingChannels: []*lnrpc.PendingChannelsResponse_ForceClosedChannel{
				{
					Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
						ChannelPoint:  "force:0",
						RemoteNodePub: "remote-force",
					},
					LimboBalance:      10_000,
					BlocksTilMaturity: 3,
					ClosingTxid:       "close-tx",
				},
			},
		},
		peers: &lnrpc.ListPeersResponse{
			Peers: []*lnrpc.Peer{{
				PubKey:    "peer",
				Address:   "127.0.0.1:9735",
				FlapCount: 10,
			}},
		},
	})

	result, err := svc.HandleNodeHealth(
		context.Background(), &mcp.CallToolRequest{},
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := decodeHealthResult(t, result)
	assert.Equal(t, "critical", out["overall_status"])
	assert.Equal(t, float64(1), out["critical_count"])
	assert.Equal(t, float64(2), out["warning_count"])

	alerts := out["alerts"].([]any)
	require.Len(t, alerts, 3)
	assert.Equal(t, "channel:force-close:force:0",
		alerts[0].(map[string]any)["id"])
	assert.Equal(t, "critical",
		alerts[0].(map[string]any)["severity"])
	assert.Equal(t, "sync:graph",
		alerts[1].(map[string]any)["id"])
	assert.Equal(t, "warning",
		alerts[1].(map[string]any)["severity"])
	assert.Equal(t, "peer:flap:peer",
		alerts[2].(map[string]any)["id"])
	assert.Equal(t, "warning",
		alerts[2].(map[string]any)["severity"])
}

func TestHealthService_WaitingCloseWithForceStatusIsCritical(t *testing.T) {
	svc := NewHealthService(&mockHealthClient{
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: "node",
			Alias:          "test-node",
			SyncedToChain:  true,
			SyncedToGraph:  true,
			BlockHeight:    42,
		},
		pending: &lnrpc.PendingChannelsResponse{
			WaitingCloseChannels: []*lnrpc.PendingChannelsResponse_WaitingCloseChannel{
				{
					Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
						ChannelPoint:    "waiting:1",
						RemoteNodePub:   "remote-waiting",
						ChanStatusFlags: "ChanStatusBorked|ChanStatusCommitBroadcasted",
					},
					LimboBalance: 5000,
					Commitments:  &lnrpc.PendingChannelsResponse_Commitments{},
					ClosingTxid:  "force-close-tx",
				},
			},
		},
		peers: &lnrpc.ListPeersResponse{},
	})

	result, err := svc.HandleNodeHealth(
		context.Background(), &mcp.CallToolRequest{},
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := decodeHealthResult(t, result)
	assert.Equal(t, "critical", out["overall_status"])
	assert.Equal(t, float64(1), out["critical_count"])
	assert.Equal(t, float64(0), out["warning_count"])

	alerts := out["alerts"].([]any)
	require.Len(t, alerts, 1)
	alert := alerts[0].(map[string]any)
	assert.Equal(t, "channel:force-close:waiting:1", alert["id"])
	assert.Equal(t, "critical", alert["severity"])
	assert.Equal(t, "Force-closing channel detected", alert["message"])
	assert.Equal(t, "force-close-tx", alert["closing_txid"])
}

func TestHealthService_OrdinaryWaitingCloseWithCommitmentsIsWarning(t *testing.T) {
	svc := NewHealthService(&mockHealthClient{
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: "node",
			Alias:          "test-node",
			SyncedToChain:  true,
			SyncedToGraph:  true,
			BlockHeight:    42,
		},
		pending: &lnrpc.PendingChannelsResponse{
			WaitingCloseChannels: []*lnrpc.PendingChannelsResponse_WaitingCloseChannel{
				{
					Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
						ChannelPoint:  "waiting:1",
						RemoteNodePub: "remote-waiting",
					},
					LimboBalance: 5000,
					Commitments:  &lnrpc.PendingChannelsResponse_Commitments{},
					ClosingTxid:  "close-tx",
				},
			},
		},
		peers: &lnrpc.ListPeersResponse{},
	})

	result, err := svc.HandleNodeHealth(
		context.Background(), &mcp.CallToolRequest{},
	)
	require.NoError(t, err)
	require.False(t, result.IsError)

	out := decodeHealthResult(t, result)
	assert.Equal(t, "degraded", out["overall_status"])
	assert.Equal(t, float64(0), out["critical_count"])
	assert.Equal(t, float64(1), out["warning_count"])

	alerts := out["alerts"].([]any)
	require.Len(t, alerts, 1)
	alert := alerts[0].(map[string]any)
	assert.Equal(t, "channel:waiting-close:waiting:1", alert["id"])
	assert.Equal(t, "warning", alert["severity"])
	assert.Equal(t, "Channel waiting to close", alert["message"])
	assert.Equal(t, "close-tx", alert["closing_txid"])
}
