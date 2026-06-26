// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"strconv"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FeeService proposes routing fee adjustments using only read-only RPCs.
type FeeService struct {
	LightningClient lnrpc.LightningClient
}

// feePageSize is the maximum number of forwarding events fetched per page.
const feePageSize uint32 = 50000

// NewFeeService creates a new FeeService.
func NewFeeService(client lnrpc.LightningClient) *FeeService {
	return &FeeService{LightningClient: client}
}

// ProposeFeesTool returns the MCP tool definition for propose_fees.
func (s *FeeService) ProposeFeesTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "lnc_propose_fees",
		Description: "Analyse routing history and channel balances to propose " +
			"fee-rate adjustments per channel. Read-only: uses " +
			"ForwardingHistory and ListChannels only.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"days": map[string]any{
					"type":        "number",
					"description": "Lookback window in days for forwarding history (default 7, max 90)",
					"minimum":     1,
					"maximum":     90,
				},
				"min_fee_ppm": map[string]any{
					"type":        "number",
					"description": "Minimum proposed fee in parts-per-million (default 1)",
					"minimum":     1,
				},
				"max_fee_ppm": map[string]any{
					"type":        "number",
					"description": "Maximum proposed fee in parts-per-million (default 5000)",
					"minimum":     1,
				},
			},
		},
	}
}

// HandleProposeFees handles the propose_fees request.
func (s *FeeService) HandleProposeFees(ctx context.Context,
	request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {

	if s.LightningClient == nil {
		return newToolResultError(
			"Not connected to Lightning node. Use lnc_connect first."), nil
	}

	args := requestArguments(request)

	days, _ := args["days"].(float64)
	if days <= 0 {
		days = 7
	}
	if days > 90 {
		days = 90
	}

	minFeePPM, _ := args["min_fee_ppm"].(float64)
	if minFeePPM <= 0 {
		minFeePPM = 1
	}
	maxFeePPM, _ := args["max_fee_ppm"].(float64)
	if maxFeePPM <= 0 {
		maxFeePPM = 5000
	}

	// Bug fix: reject inverted bounds before making any API calls.
	if minFeePPM > maxFeePPM {
		return newToolResultError(
			"min_fee_ppm must not exceed max_fee_ppm"), nil
	}

	// Bug fix: convert fractional days to nanoseconds correctly so that
	// values like 1.5 produce the right duration rather than truncating to
	// the nearest integer day.
	startTime := uint64(
		time.Now().Add(
			-time.Duration(days * 24 * float64(time.Hour)),
		).Unix(),
	)

	// Bug fix: paginate ForwardingHistory so nodes with more than 50 000
	// events in the window are not silently truncated.
	type chanStats struct {
		forwardsOut  int64
		amtOutMsat   int64
		feesEarnedMs int64
	}
	stats := make(map[uint64]*chanStats)
	totalForwards := 0
	var offset uint32
	for {
		batch, err := s.LightningClient.ForwardingHistory(
			ctx, &lnrpc.ForwardingHistoryRequest{
				StartTime:    startTime,
				NumMaxEvents: feePageSize,
				IndexOffset:  offset,
			},
		)
		if err != nil {
			return newToolResultError(
				"Failed to fetch forwarding history: " + err.Error()), nil
		}

		totalForwards += len(batch.ForwardingEvents)
		for _, ev := range batch.ForwardingEvents {
			if _, ok := stats[ev.ChanIdOut]; !ok {
				stats[ev.ChanIdOut] = &chanStats{}
			}
			s2 := stats[ev.ChanIdOut]
			s2.forwardsOut++
			s2.amtOutMsat += int64(ev.AmtOutMsat)
			s2.feesEarnedMs += int64(ev.FeeMsat)
		}

		if uint32(len(batch.ForwardingEvents)) < feePageSize {
			break
		}
		offset = batch.LastOffsetIndex
	}

	channels, err := s.LightningClient.ListChannels(
		ctx, &lnrpc.ListChannelsRequest{},
	)
	if err != nil {
		return newToolResultError(
			"Failed to list channels: " + err.Error()), nil
	}

	proposals := make([]map[string]any, 0, len(channels.Channels))
	for _, ch := range channels.Channels {
		localRatio := float64(0)
		if ch.Capacity > 0 {
			localRatio = float64(ch.LocalBalance) / float64(ch.Capacity)
		}

		st := stats[ch.ChanId]
		forwards := int64(0)
		if st != nil {
			forwards = st.forwardsOut
		}

		proposedPPM := proposeFee(localRatio, forwards, minFeePPM, maxFeePPM)

		entry := map[string]any{
			"chan_id":          strconv.FormatUint(ch.ChanId, 10),
			"remote_pubkey":    ch.RemotePubkey,
			"channel_point":    ch.ChannelPoint,
			"capacity_sat":     ch.Capacity,
			"local_balance":    ch.LocalBalance,
			"remote_balance":   ch.RemoteBalance,
			"local_ratio":      round2(localRatio),
			"forwards_out":     forwards,
			"proposed_fee_ppm": proposedPPM,
			"reason":           feeReason(localRatio, forwards),
		}
		if st != nil {
			entry["amt_routed_msat"] = st.amtOutMsat
			entry["fees_earned_msat"] = st.feesEarnedMs
		}
		proposals = append(proposals, entry)
	}

	return newToolResultJSON(map[string]any{
		"lookback_days":  days,
		"total_forwards": totalForwards,
		"proposals":      proposals,
		"note":           "Proposals are suggestions only. Apply with UpdateChannelPolicy after review.",
	}), nil
}

// proposeFee returns a fee in PPM based on local liquidity ratio and recent
// forward count.
//
// Strategy:
//   - Depleted channel (localRatio < 0.2): raise fees to slow outflow.
//   - Saturated channel (localRatio > 0.8): lower fees to encourage outflow.
//   - Idle channel (0 forwards in window): lower fees to attract routing.
//   - Active balanced channel: scale proportionally between min and max.
func proposeFee(localRatio float64, forwards int64, minPPM, maxPPM float64) int64 {
	var ppm float64
	switch {
	case localRatio < 0.2:
		// Bug fix: use range-relative formula (same pattern as all other
		// branches) so the result respects minPPM and scales with the range.
		ppm = minPPM + (maxPPM-minPPM)*0.90
	case localRatio > 0.8:
		// Over-full — lower fee to encourage outflow and rebalancing.
		ppm = minPPM + (maxPPM-minPPM)*0.10
	case forwards == 0:
		// Idle channel — offer a discount to attract first routes.
		ppm = minPPM + (maxPPM-minPPM)*0.15
	default:
		// Balanced and routing — linear scale: higher local ratio → lower fee.
		ppm = minPPM + (maxPPM-minPPM)*(1-localRatio)
	}

	if ppm < minPPM {
		ppm = minPPM
	}
	if ppm > maxPPM {
		ppm = maxPPM
	}
	return int64(ppm)
}

func feeReason(localRatio float64, forwards int64) string {
	switch {
	case localRatio < 0.2:
		return "depleted: raised fee to slow outflow"
	case localRatio > 0.8:
		return "saturated: lowered fee to encourage outflow"
	case forwards == 0:
		return "idle: discounted fee to attract routing"
	default:
		return "balanced and active: fee scaled by local liquidity ratio"
	}
}

func round2(f float64) float64 {
	return float64(int(f*100)) / 100
}
