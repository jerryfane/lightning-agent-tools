// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NodeOpsRebalanceService submits bounded rebalance requests to
// node-ops-daemon. The MCP server remains a thin client and never receives
// write credentials.
type NodeOpsRebalanceService struct {
	DaemonSocket string
	DialTimeout  time.Duration
	callDaemon   nodeOpsDaemonCaller
}

// NewNodeOpsRebalanceService creates the gated rebalance MCP service.
func NewNodeOpsRebalanceService(socketPath string) *NodeOpsRebalanceService {
	return &NodeOpsRebalanceService{
		DaemonSocket: socketPath,
		DialTimeout:  defaultNodeOpsDialDelay,
		callDaemon:   callNodeOpsDaemon,
	}
}

// ExecuteRebalanceTool returns the MCP tool definition.
func (s *NodeOpsRebalanceService) ExecuteRebalanceTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "lnc_execute_rebalance",
		Description: "Submit a gated circular rebalance request to node-ops-daemon. " +
			"The daemon enforces budgets, max fee ppm, cooldowns, approval, kill-switch, and audit logging before any LND write.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"outgoing_chan_id": map[string]any{
					"type":        []string{"integer", "string"},
					"description": "Local channel id that must carry the outgoing HTLC.",
				},
				"incoming_chan_id": map[string]any{
					"type":        []string{"integer", "string"},
					"description": "Local channel id that must carry the incoming HTLC.",
				},
				"amount_sat": map[string]any{
					"type":        "integer",
					"description": "Amount to rebalance in satoshis.",
					"minimum":     1,
				},
				"max_fee_ppm": map[string]any{
					"type":        "integer",
					"description": "Maximum route fee rate in parts-per-million.",
					"minimum":     0,
				},
			},
			Required: []string{
				"outgoing_chan_id",
				"incoming_chan_id",
				"amount_sat",
				"max_fee_ppm",
			},
		},
	}
}

// HandleExecuteRebalance forwards the request to the governance daemon.
func (s *NodeOpsRebalanceService) HandleExecuteRebalance(
	ctx context.Context,
	request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {

	args := requestArguments(request)
	outgoingChanID, err := requiredUint64Arg(args, "outgoing_chan_id")
	if err != nil {
		return newToolResultError(err.Error()), nil
	}
	incomingChanID, err := requiredUint64Arg(args, "incoming_chan_id")
	if err != nil {
		return newToolResultError(err.Error()), nil
	}
	amountSat, err := requiredInt64Arg(args, "amount_sat", 1, maxInt64Arg)
	if err != nil {
		return newToolResultError(err.Error()), nil
	}
	maxFeePPM, err := requiredInt64Arg(args, "max_fee_ppm", 0, maxInt64Arg)
	if err != nil {
		return newToolResultError(err.Error()), nil
	}

	call := s.callDaemon
	if call == nil {
		call = callNodeOpsDaemon
	}
	resp, err := call(
		ctx, s.DaemonSocket, s.DialTimeout, "execute_rebalance",
		map[string]any{
			"outgoing_chan_id": outgoingChanID,
			"incoming_chan_id": incomingChanID,
			"amount_sat":       amountSat,
			"max_fee_ppm":      maxFeePPM,
		},
	)
	if err != nil {
		return newToolResultError(
			"Failed to submit gated rebalance request: " + err.Error()), nil
	}
	if resp == nil {
		return newToolResultError(
			"Failed to submit gated rebalance request: empty daemon response"), nil
	}
	if resp.Status == "error" {
		reason := resp.Reason
		if reason == "" {
			reason = "node-ops daemon returned status error"
		}
		return newToolResultError(reason), nil
	}
	return newToolResultJSON(nodeOpsToolResponse(resp)), nil
}
