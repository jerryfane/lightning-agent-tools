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
	assert.Equal(t, "critical",
		alerts[0].(map[string]any)["severity"])
	assert.Equal(t, "warning",
		alerts[1].(map[string]any)["severity"])
	assert.Equal(t, "warning",
		alerts[2].(map[string]any)["severity"])
}
