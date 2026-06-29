// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HealthService aggregates node health data into prioritized alerts.
type HealthService struct {
	LightningClient lnrpc.LightningClient
}

// NewHealthService creates a new health service.
func NewHealthService(client lnrpc.LightningClient) *HealthService {
	return &HealthService{
		LightningClient: client,
	}
}

// NodeHealthTool returns the MCP tool definition for node health alerts.
func (s *HealthService) NodeHealthTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "lnc_node_health",
		Description: "Aggregate node health alerts: force-closing channels, " +
			"peer flap counts, and sync status into a prioritized list",
		InputSchema: ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}
}

// HandleNodeHealth handles the node health request.
func (s *HealthService) HandleNodeHealth(ctx context.Context,
	request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {

	if s.LightningClient == nil {
		return newToolResultError(
			"Not connected to Lightning node. " +
				"Use lnc_connect first."), nil
	}

	alerts := make([]map[string]any, 0)

	// --- sync status (from GetInfo) ---
	info, err := s.LightningClient.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return newToolResultError(
			"Failed to get node info: " + err.Error()), nil
	}

	if !info.SyncedToChain {
		alerts = append(alerts, map[string]any{
			"id":       "sync:chain",
			"severity": "critical",
			"category": "sync",
			"message":  "Node is NOT synced to chain",
		})
	}
	if !info.SyncedToGraph {
		alerts = append(alerts, map[string]any{
			"id":       "sync:graph",
			"severity": "warning",
			"category": "sync",
			"message":  "Node is NOT synced to graph",
		})
	}

	// --- force-closing / waiting-close channels (from PendingChannels) ---
	pending, err := s.LightningClient.PendingChannels(
		ctx, &lnrpc.PendingChannelsRequest{},
	)
	if err != nil {
		return newToolResultError(
			"Failed to get pending channels: " + err.Error()), nil
	}

	for _, ch := range pending.PendingForceClosingChannels {
		alerts = append(alerts, map[string]any{
			"id":                  healthAlertID("channel:force-close", ch.Channel.ChannelPoint),
			"severity":            "critical",
			"category":            "channel",
			"message":             "Force-closing channel detected",
			"channel_point":       ch.Channel.ChannelPoint,
			"remote_node_pub":     ch.Channel.RemoteNodePub,
			"limbo_balance_sat":   ch.LimboBalance,
			"blocks_til_maturity": ch.BlocksTilMaturity,
			"closing_txid":        ch.ClosingTxid,
		})
	}

	for _, ch := range pending.WaitingCloseChannels {
		idPrefix := "channel:waiting-close"
		severity := "warning"
		message := "Channel waiting to close"
		if waitingCloseLooksForceClosed(ch) {
			idPrefix = "channel:force-close"
			severity = "critical"
			message = "Force-closing channel detected"
		}
		alerts = append(alerts, map[string]any{
			"id":                healthAlertID(idPrefix, ch.Channel.ChannelPoint),
			"severity":          severity,
			"category":          "channel",
			"message":           message,
			"channel_point":     ch.Channel.ChannelPoint,
			"remote_node_pub":   ch.Channel.RemoteNodePub,
			"limbo_balance_sat": ch.LimboBalance,
			"chan_status_flags": ch.Channel.ChanStatusFlags,
			"closing_txid":      ch.ClosingTxid,
		})
	}

	// --- peer flap counts (from ListPeers) ---
	const flapThreshold = int32(10)
	peers, err := s.LightningClient.ListPeers(
		ctx, &lnrpc.ListPeersRequest{},
	)
	if err != nil {
		return newToolResultError(
			"Failed to list peers: " + err.Error()), nil
	}

	for _, peer := range peers.Peers {
		if peer.FlapCount >= flapThreshold {
			alerts = append(alerts, map[string]any{
				"id":         healthAlertID("peer:flap", peer.PubKey),
				"severity":   "warning",
				"category":   "peer",
				"message":    fmt.Sprintf("Peer has high flap count (%d)", peer.FlapCount),
				"pub_key":    peer.PubKey,
				"address":    peer.Address,
				"flap_count": peer.FlapCount,
			})
		}
	}

	sort.SliceStable(alerts, func(i, j int) bool {
		return healthSeverityRank(alerts[i]["severity"]) <
			healthSeverityRank(alerts[j]["severity"])
	})

	// Summarize counts per severity.
	critical, warning := 0, 0
	for _, a := range alerts {
		switch a["severity"] {
		case "critical":
			critical++
		case "warning":
			warning++
		}
	}

	overall := "healthy"
	if critical > 0 {
		overall = "critical"
	} else if warning > 0 {
		overall = "degraded"
	}

	return newToolResultJSON(map[string]any{
		"overall_status":  overall,
		"alert_count":     len(alerts),
		"critical_count":  critical,
		"warning_count":   warning,
		"alerts":          alerts,
		"node_id":         info.IdentityPubkey,
		"alias":           info.Alias,
		"synced_to_chain": info.SyncedToChain,
		"synced_to_graph": info.SyncedToGraph,
		"block_height":    info.BlockHeight,
	}), nil
}

func healthAlertID(prefix, key string) string {
	if key == "" {
		return prefix
	}
	return prefix + ":" + key
}

func healthSeverityRank(severity any) int {
	switch severity {
	case "critical":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

func waitingCloseLooksForceClosed(
	ch *lnrpc.PendingChannelsResponse_WaitingCloseChannel) bool {

	if ch == nil {
		return false
	}
	flags := ch.GetChannel().GetChanStatusFlags()
	return strings.Contains(flags, "ChanStatusBorked") ||
		strings.Contains(flags, "ChanStatusCommitBroadcasted")
}
