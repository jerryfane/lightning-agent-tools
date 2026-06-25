// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultLookbackDays    = 30
	maxForwardingEvents    = 50000
	minChannelCountForOpen = 5
)

// ChannelActionsService proposes read-only channel open/close candidates.
type ChannelActionsService struct {
	LightningClient lnrpc.LightningClient
}

// NewChannelActionsService creates a new channel actions service.
func NewChannelActionsService(
	client lnrpc.LightningClient) *ChannelActionsService {

	return &ChannelActionsService{LightningClient: client}
}

// ProposeChannelActionsTool returns the MCP tool definition.
func (s *ChannelActionsService) ProposeChannelActionsTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "lnc_propose_channel_actions",
		Description: "Propose read-only channel open/close candidates. " +
			"Flags unproductive or inactive channels as close candidates " +
			"and suggests well-connected peers as open candidates based on " +
			"forwarding history and the network graph. " +
			"Read-only — no changes are made to the node.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"lookback_days": map[string]any{
					"type": "integer",
					"description": "Days to look back for forwarding " +
						"activity (default 30)",
					"minimum": 1,
					"maximum": 365,
				},
				"max_close_candidates": map[string]any{
					"type": "integer",
					"description": "Max close candidates to return " +
						"(default 10)",
					"minimum": 1,
					"maximum": 50,
				},
				"max_open_candidates": map[string]any{
					"type": "integer",
					"description": "Max open candidates to return " +
						"(default 5)",
					"minimum": 1,
					"maximum": 20,
				},
			},
		},
	}
}

// HandleProposeChannelActions handles the propose channel actions request.
func (s *ChannelActionsService) HandleProposeChannelActions(
	ctx context.Context,
	request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {

	args := requestArguments(request)

	if s.LightningClient == nil {
		return newToolResultError(
			"Not connected to Lightning node. " +
				"Use lnc_connect first."), nil
	}

	lookbackDays := defaultLookbackDays
	if v, ok := args["lookback_days"].(float64); ok && v >= 1 {
		lookbackDays = int(v)
	}
	maxClose := 10
	if v, ok := args["max_close_candidates"].(float64); ok && v >= 1 {
		maxClose = int(v)
	}
	maxOpen := 5
	if v, ok := args["max_open_candidates"].(float64); ok && v >= 1 {
		maxOpen = int(v)
	}

	// Fetch all open channels.
	chanResp, err := s.LightningClient.ListChannels(
		ctx, &lnrpc.ListChannelsRequest{},
	)
	if err != nil {
		return newToolResultError(
			"Failed to list channels: " + err.Error()), nil
	}

	// Fetch forwarding history for the lookback window.
	now := uint64(time.Now().Unix())
	fwdResp, err := s.LightningClient.ForwardingHistory(
		ctx, &lnrpc.ForwardingHistoryRequest{
			StartTime:    now - uint64(lookbackDays*86400),
			EndTime:      now,
			NumMaxEvents: maxForwardingEvents,
		},
	)
	if err != nil {
		return newToolResultError(
			"Failed to get forwarding history: " + err.Error()), nil
	}

	// Build set of channel IDs seen in forwarding events.
	activeFwdChans := make(map[uint64]bool)
	for _, evt := range fwdResp.ForwardingEvents {
		activeFwdChans[evt.ChanIdIn] = true
		activeFwdChans[evt.ChanIdOut] = true
	}

	noFwdReason := "no_forwards_in_" + strconv.Itoa(lookbackDays) + "d"

	// Score and collect close candidates.
	type closeEntry struct {
		ch      *lnrpc.Channel
		reasons []string
		score   int
	}
	var closeEntries []closeEntry
	existingPeers := make(map[string]bool)

	for _, ch := range chanResp.Channels {
		existingPeers[ch.RemotePubkey] = true

		var reasons []string
		score := 0

		if !ch.Active {
			reasons = append(reasons, "inactive_channel")
			score += 3
		}
		if ch.NumUpdates == 0 &&
			ch.TotalSatoshisSent == 0 &&
			ch.TotalSatoshisReceived == 0 {
			reasons = append(reasons, "zero_activity")
			score += 2
		}
		if !activeFwdChans[ch.ChanId] {
			reasons = append(reasons, noFwdReason)
			score += 1
		}

		if len(reasons) > 0 {
			closeEntries = append(closeEntries, closeEntry{
				ch:      ch,
				reasons: reasons,
				score:   score,
			})
		}
	}

	sort.Slice(closeEntries, func(i, j int) bool {
		return closeEntries[i].score > closeEntries[j].score
	})
	if len(closeEntries) > maxClose {
		closeEntries = closeEntries[:maxClose]
	}

	closeResult := make([]map[string]any, len(closeEntries))
	for i, e := range closeEntries {
		closeResult[i] = map[string]any{
			"chan_id":         strconv.FormatUint(e.ch.ChanId, 10),
			"channel_point":   e.ch.ChannelPoint,
			"remote_pubkey":   e.ch.RemotePubkey,
			"capacity":        e.ch.Capacity,
			"local_balance":   e.ch.LocalBalance,
			"remote_balance":  e.ch.RemoteBalance,
			"active":          e.ch.Active,
			"num_updates":     e.ch.NumUpdates,
			"total_sats_sent": e.ch.TotalSatoshisSent,
			"total_sats_recv": e.ch.TotalSatoshisReceived,
			"reasons":         e.reasons,
		}
	}

	// Open candidates: well-connected nodes not already peered with.
	openResult := buildOpenCandidates(
		ctx, s.LightningClient, existingPeers, maxOpen,
	)

	return newToolResultJSON(map[string]any{
		"close_candidates":     closeResult,
		"open_candidates":      openResult,
		"analysis_window_days": lookbackDays,
		"total_channels":       len(chanResp.Channels),
		"forwarding_events":    len(fwdResp.ForwardingEvents),
	}), nil
}

// buildOpenCandidates returns well-connected nodes from the network graph
// that the local node does not already have channels with.
func buildOpenCandidates(
	ctx context.Context,
	client lnrpc.LightningClient,
	existingPeers map[string]bool,
	maxCandidates int,
) []map[string]any {

	graph, err := client.DescribeGraph(
		ctx, &lnrpc.ChannelGraphRequest{IncludeUnannounced: false},
	)
	if err != nil {
		return nil
	}

	nodeChanCount := make(map[string]int)
	nodeCapacity := make(map[string]int64)
	for _, edge := range graph.Edges {
		nodeChanCount[edge.Node1Pub]++
		nodeChanCount[edge.Node2Pub]++
		nodeCapacity[edge.Node1Pub] += edge.Capacity
		nodeCapacity[edge.Node2Pub] += edge.Capacity
	}

	aliasMap := make(map[string]string)
	for _, node := range graph.Nodes {
		aliasMap[node.PubKey] = node.Alias
	}

	type candidate struct {
		pubKey   string
		alias    string
		numChans int
		capacity int64
	}

	var candidates []candidate
	for pubKey, numChans := range nodeChanCount {
		if existingPeers[pubKey] || numChans < minChannelCountForOpen {
			continue
		}
		candidates = append(candidates, candidate{
			pubKey:   pubKey,
			alias:    aliasMap[pubKey],
			numChans: numChans,
			capacity: nodeCapacity[pubKey],
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].numChans > candidates[j].numChans
	})
	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates]
	}

	result := make([]map[string]any, len(candidates))
	for i, c := range candidates {
		result[i] = map[string]any{
			"peer_pubkey":    c.pubKey,
			"alias":          c.alias,
			"num_channels":   c.numChans,
			"total_capacity": c.capacity,
			"reasons":        []string{"well_connected_node"},
		}
	}
	return result
}
