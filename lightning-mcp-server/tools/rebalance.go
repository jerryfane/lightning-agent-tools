// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"
)

const (
	defaultRebalanceThreshold    = 0.65
	defaultMaxRebalanceCandidates = 10
	defaultRebalanceMaxFeePPM    = 500
	minRebalanceAmountSat        = 20_000
	fwdHistoryLookbackDays       = 30
)

// rebalanceClient is the narrow read-only interface used by RebalanceService.
// The method signatures match lnrpc.LightningClient so any gRPC-generated
// client satisfies this interface without wrapping.
type rebalanceClient interface {
	ListChannels(context.Context, *lnrpc.ListChannelsRequest,
		...grpc.CallOption) (*lnrpc.ListChannelsResponse, error)
	ForwardingHistory(context.Context, *lnrpc.ForwardingHistoryRequest,
		...grpc.CallOption) (*lnrpc.ForwardingHistoryResponse, error)
}

// chanSnapshot holds per-channel data for rebalance analysis.
type chanSnapshot struct {
	chanID    uint64
	chanIDStr string
	remotePub string
	capacity  int64
	local     int64
	remote    int64
	ratio     float64
	// fwdNetSat is net outbound sats over the lookback window
	// (positive means the channel sent more than it received).
	fwdNetSat int64
}

// RebalanceService detects imbalanced channels and proposes rebalancing.
type RebalanceService struct {
	LightningClient rebalanceClient
}

// NewRebalanceService creates a new rebalance service.
func NewRebalanceService(client rebalanceClient) *RebalanceService {
	return &RebalanceService{LightningClient: client}
}

// ProposeRebalanceTool returns the MCP tool definition for lnc_propose_rebalance.
func (s *RebalanceService) ProposeRebalanceTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "lnc_propose_rebalance",
		Description: "Detect imbalanced Lightning channels and propose circular " +
			"rebalance candidates. Read-only — returns proposals only, does not " +
			"move funds.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"imbalance_threshold": map[string]any{
					"type": "number",
					"description": "Local-balance ratio above which a channel is " +
						"over-local (mirrored for over-remote). " +
						"Range 0.5–0.95; default 0.65.",
					"default": 0.65,
				},
				"max_candidates": map[string]any{
					"type":        "integer",
					"description": "Maximum candidates to return (default 10).",
					"default":     10,
				},
				"max_fee_ppm": map[string]any{
					"type": "integer",
					"description": "Suggested maximum rebalance fee in " +
						"parts-per-million (default 500).",
					"default": 500,
				},
				"include_forwarding_demand": map[string]any{
					"type": "boolean",
					"description": "Enrich candidates with 30-day forwarding " +
						"volume to highlight demand-driven urgency (default true).",
					"default": true,
				},
			},
		},
	}
}

// HandleProposeRebalance handles the lnc_propose_rebalance tool call.
func (s *RebalanceService) HandleProposeRebalance(ctx context.Context,
	request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {

	if s.LightningClient == nil {
		return newToolResultError(
			"Not connected to Lightning node. Use lnc_connect first."), nil
	}

	args := requestArguments(request)

	threshold := defaultRebalanceThreshold
	if v, ok := args["imbalance_threshold"].(float64); ok && v > 0.5 && v < 1.0 {
		threshold = v
	}
	maxCandidates := defaultMaxRebalanceCandidates
	if v, ok := args["max_candidates"].(float64); ok && int(v) > 0 {
		maxCandidates = int(v)
	}
	maxFeePPM := int64(defaultRebalanceMaxFeePPM)
	if v, ok := args["max_fee_ppm"].(float64); ok && v > 0 {
		maxFeePPM = int64(v)
	}
	includeFwd := true
	if v, ok := args["include_forwarding_demand"].(bool); ok {
		includeFwd = v
	}

	chanResp, err := s.LightningClient.ListChannels(
		ctx, &lnrpc.ListChannelsRequest{ActiveOnly: true},
	)
	if err != nil {
		return newToolResultError(
			"Failed to list channels: " + err.Error()), nil
	}

	fwdNet := map[uint64]int64{}
	if includeFwd {
		fwdNet = s.forwardingNetFlow(ctx)
	}

	var overLocal, overRemote []chanSnapshot
	for _, ch := range chanResp.Channels {
		if ch.Capacity == 0 {
			continue
		}
		ratio := float64(ch.LocalBalance) / float64(ch.Capacity)
		snap := chanSnapshot{
			chanID:    ch.ChanId,
			chanIDStr: strconv.FormatUint(ch.ChanId, 10),
			remotePub: ch.RemotePubkey,
			capacity:  ch.Capacity,
			local:     ch.LocalBalance,
			remote:    ch.RemoteBalance,
			ratio:     ratio,
			fwdNetSat: fwdNet[ch.ChanId],
		}
		switch {
		case ratio > threshold:
			overLocal = append(overLocal, snap)
		case ratio < (1.0 - threshold):
			overRemote = append(overRemote, snap)
		}
	}

	sortSnapshots(overLocal, true)  // descending: most over-local first
	sortSnapshots(overRemote, false) // ascending: most over-remote first

	type rebalanceCandidate struct {
		OutChan   string `json:"out_chan"`
		InChan    string `json:"in_chan"`
		AmountSat int64  `json:"amount_sat"`
		MaxFeePPM int64  `json:"max_fee_ppm"`
		Reason    string `json:"reason"`
	}

	candidates := make([]rebalanceCandidate, 0)
outer:
	for _, ol := range overLocal {
		for _, ir := range overRemote {
			if len(candidates) >= maxCandidates {
				break outer
			}
			excessLocal := ol.local - ol.capacity/2
			deficitLocal := ir.capacity/2 - ir.local
			amount := min64(excessLocal, deficitLocal)
			if amount < minRebalanceAmountSat {
				continue
			}

			reason := fmt.Sprintf(
				"out_chan %.0f%% local (excess %d sat); "+
					"in_chan %.0f%% local (deficit %d sat)",
				ol.ratio*100, excessLocal, ir.ratio*100, deficitLocal,
			)
			if includeFwd {
				if ol.fwdNetSat < 0 {
					reason += fmt.Sprintf(
						"; out_chan has net inbound pressure (%d sat net), "+
							"rebalance is urgent",
						ol.fwdNetSat,
					)
				}
				if ir.fwdNetSat > 0 {
					reason += fmt.Sprintf(
						"; in_chan has net outbound pressure (+%d sat net), "+
							"rebalance is urgent",
						ir.fwdNetSat,
					)
				}
			}

			candidates = append(candidates, rebalanceCandidate{
				OutChan:   ol.chanIDStr,
				InChan:    ir.chanIDStr,
				AmountSat: amount,
				MaxFeePPM: maxFeePPM,
				Reason:    reason,
			})
		}
	}

	return newToolResultJSON(map[string]any{
		"candidates": candidates,
		"analysis": map[string]any{
			"over_local_channels":   snapsToMaps(overLocal, includeFwd),
			"over_remote_channels":  snapsToMaps(overRemote, includeFwd),
			"threshold":             threshold,
			"total_active_channels": len(chanResp.Channels),
		},
	}), nil
}

// forwardingNetFlow fetches 30-day forwarding history and returns a per-channel
// map of net outbound satoshis. Returns an empty map on error so callers
// degrade gracefully.
func (s *RebalanceService) forwardingNetFlow(
	ctx context.Context) map[uint64]int64 {

	now := time.Now()
	resp, err := s.LightningClient.ForwardingHistory(ctx,
		&lnrpc.ForwardingHistoryRequest{
			StartTime: uint64(
				now.Add(-fwdHistoryLookbackDays * 24 * time.Hour).Unix(),
			),
			EndTime:      uint64(now.Unix()),
			NumMaxEvents: 50_000,
		},
	)
	if err != nil {
		return map[uint64]int64{}
	}
	net := make(map[uint64]int64, len(resp.ForwardingEvents))
	for _, ev := range resp.ForwardingEvents {
		net[ev.ChanIdOut] += int64(ev.AmtOut)
		net[ev.ChanIdIn] -= int64(ev.AmtIn)
	}
	return net
}

// sortSnapshots sorts channel snapshots by ratio, descending if desc=true.
func sortSnapshots(snaps []chanSnapshot, desc bool) {
	for i := 1; i < len(snaps); i++ {
		for j := i; j > 0; j-- {
			swap := desc && snaps[j].ratio > snaps[j-1].ratio ||
				!desc && snaps[j].ratio < snaps[j-1].ratio
			if !swap {
				break
			}
			snaps[j], snaps[j-1] = snaps[j-1], snaps[j]
		}
	}
}

// snapsToMaps converts channel snapshots to JSON-serialisable maps.
func snapsToMaps(snaps []chanSnapshot, includeFwd bool) []map[string]any {
	out := make([]map[string]any, len(snaps))
	for i, sn := range snaps {
		m := map[string]any{
			"chan_id":         sn.chanIDStr,
			"remote_pubkey":  sn.remotePub,
			"capacity":       sn.capacity,
			"local_balance":  sn.local,
			"remote_balance": sn.remote,
			"local_ratio":    fmt.Sprintf("%.2f", sn.ratio),
		}
		if includeFwd {
			m["fwd_net_flow_sat"] = sn.fwdNetSat
		}
		out[i] = m
	}
	return out
}

// min64 returns the smaller of two int64 values.
func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
