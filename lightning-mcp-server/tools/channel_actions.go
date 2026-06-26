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
	defaultLookbackDays           = 30
	maxLookbackDays               = 365
	channelActionsHistoryPageSize = uint32(50000)
	minChannelCountForOpen        = 5
	defaultCloseCandidates        = 10
	maxCloseCandidates            = 50
	defaultOpenCandidates         = 5
	maxOpenCandidates             = 20
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

	lookbackDays := boundedIntArg(
		args, "lookback_days", defaultLookbackDays, 1,
		maxLookbackDays,
	)
	maxClose := boundedIntArg(
		args, "max_close_candidates", defaultCloseCandidates, 1,
		maxCloseCandidates,
	)
	maxOpen := boundedIntArg(
		args, "max_open_candidates", defaultOpenCandidates, 1,
		maxOpenCandidates,
	)

	// Fetch all open channels.
	chanResp, err := s.LightningClient.ListChannels(
		ctx, &lnrpc.ListChannelsRequest{},
	)
	if err != nil {
		return newToolResultError(
			"Failed to list channels: " + err.Error()), nil
	}

	now := uint64(time.Now().Unix())
	startTime := now - uint64(lookbackDays)*86400

	activeFwdChans := make(map[uint64]bool)
	forwardingEvents := 0
	var offset uint32
	for {
		fwdResp, err := s.LightningClient.ForwardingHistory(
			ctx, &lnrpc.ForwardingHistoryRequest{
				StartTime:    startTime,
				EndTime:      now,
				NumMaxEvents: channelActionsHistoryPageSize,
				IndexOffset:  offset,
			},
		)
		if err != nil {
			return newToolResultError(
				"Failed to get forwarding history: " +
					err.Error()), nil
		}

		forwardingEvents += len(fwdResp.ForwardingEvents)
		for _, evt := range fwdResp.ForwardingEvents {
			activeFwdChans[evt.ChanIdIn] = true
			activeFwdChans[evt.ChanIdOut] = true
		}
		if uint32(len(fwdResp.ForwardingEvents)) <
			channelActionsHistoryPageSize {
			break
		}
		if fwdResp.LastOffsetIndex <= offset {
			return newToolResultError(
				"Failed to paginate forwarding history: " +
					"non-advancing offset"), nil
		}
		offset = fwdResp.LastOffsetIndex
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
		if closeEntries[i].score == closeEntries[j].score {
			return closeEntries[i].ch.ChanId < closeEntries[j].ch.ChanId
		}
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
	openResult, openErr := buildOpenCandidates(
		ctx, s.LightningClient, existingPeers, maxOpen,
	)

	response := map[string]any{
		"close_candidates":     closeResult,
		"open_candidates":      openResult,
		"analysis_window_days": lookbackDays,
		"total_channels":       len(chanResp.Channels),
		"forwarding_events":    forwardingEvents,
	}
	if openErr != nil {
		response["open_candidates_error"] = openErr.Error()
	}

	return newToolResultJSON(response), nil
}

// buildOpenCandidates returns well-connected nodes from the network graph
// that the local node does not already have channels with.
func buildOpenCandidates(
	ctx context.Context,
	client lnrpc.LightningClient,
	existingPeers map[string]bool,
	maxCandidates int,
) ([]map[string]any, error) {

	info, err := client.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return []map[string]any{}, err
	}

	graph, err := client.DescribeGraph(
		ctx, &lnrpc.ChannelGraphRequest{IncludeUnannounced: false},
	)
	if err != nil {
		return []map[string]any{}, err
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

	excludedPeers := make(map[string]bool, len(existingPeers)+1)
	for peer := range existingPeers {
		excludedPeers[peer] = true
	}
	excludedPeers[info.IdentityPubkey] = true

	type candidate struct {
		pubKey   string
		alias    string
		numChans int
		capacity int64
	}

	var candidates []candidate
	for pubKey, numChans := range nodeChanCount {
		if excludedPeers[pubKey] || numChans < minChannelCountForOpen {
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
		if candidates[i].numChans != candidates[j].numChans {
			return candidates[i].numChans > candidates[j].numChans
		}
		if candidates[i].capacity != candidates[j].capacity {
			return candidates[i].capacity > candidates[j].capacity
		}
		return candidates[i].pubKey < candidates[j].pubKey
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
	return result, nil
}

func boundedIntArg(args map[string]any, key string, defaultValue, minValue,
	maxValue int) int {

	value := defaultValue
	if raw, ok := args[key].(float64); ok {
		value = int(raw)
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
