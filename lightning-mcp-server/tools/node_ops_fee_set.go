// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxSafeJSONInteger = 1 << 53
	maxInt64Arg        = int64(1<<63 - 1)
	maxUint32Int64     = int64(1<<32 - 1)
)

// NodeOpsFeeSetService submits bounded fee-set requests to node-ops-daemon.
// The MCP server remains a thin client and never receives write credentials.
type NodeOpsFeeSetService struct {
	DaemonSocket string
	DialTimeout  time.Duration
	callDaemon   nodeOpsDaemonCaller
}

// NewNodeOpsFeeSetService creates the gated fee-set MCP service.
func NewNodeOpsFeeSetService(socketPath string) *NodeOpsFeeSetService {
	return &NodeOpsFeeSetService{
		DaemonSocket: socketPath,
		DialTimeout:  defaultNodeOpsDialDelay,
		callDaemon:   callNodeOpsDaemon,
	}
}

// ExecuteFeeSetTool returns the MCP tool definition.
func (s *NodeOpsFeeSetService) ExecuteFeeSetTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "lnc_execute_fee_set",
		Description: "Submit a gated fee policy update to node-ops-daemon. " +
			"The daemon enforces caps, cooldowns, approval, kill-switch, and audit logging before any LND write.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"chan_id": map[string]any{
					"type":        []string{"integer", "string"},
					"description": "Short channel id to update.",
				},
				"base_msat": map[string]any{
					"type":        "integer",
					"description": "New base forwarding fee in millisatoshis.",
					"minimum":     0,
				},
				"fee_ppm": map[string]any{
					"type":        "integer",
					"description": "New proportional forwarding fee in parts-per-million.",
					"minimum":     0,
				},
			},
			Required: []string{"chan_id", "base_msat", "fee_ppm"},
		},
	}
}

// HandleExecuteFeeSet forwards the request to the governance daemon.
func (s *NodeOpsFeeSetService) HandleExecuteFeeSet(
	ctx context.Context,
	request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {

	args := requestArguments(request)
	chanID, err := requiredUint64Arg(args, "chan_id")
	if err != nil {
		return newToolResultError(err.Error()), nil
	}
	baseMsat, err := requiredInt64Arg(args, "base_msat", 0, maxInt64Arg)
	if err != nil {
		return newToolResultError(err.Error()), nil
	}
	feePPM, err := requiredInt64Arg(args, "fee_ppm", 0, maxUint32Int64)
	if err != nil {
		return newToolResultError(err.Error()), nil
	}

	call := s.callDaemon
	if call == nil {
		call = callNodeOpsDaemon
	}
	resp, err := call(
		ctx, s.DaemonSocket, s.DialTimeout, "execute_fee_set",
		map[string]any{
			"chan_id":   chanID,
			"base_msat": baseMsat,
			"fee_ppm":   feePPM,
		},
	)
	if err != nil {
		return newToolResultError(
			"Failed to submit gated fee-set request: " + err.Error()), nil
	}
	if resp == nil {
		return newToolResultError(
			"Failed to submit gated fee-set request: empty daemon response"), nil
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

func nodeOpsToolResponse(resp *nodeOpsDaemonResponse) map[string]any {
	out := map[string]any{
		"status":     resp.Status,
		"request_id": resp.RequestID,
	}
	if resp.Reason != "" {
		out["reason"] = resp.Reason
	}
	if len(resp.Result) > 0 {
		var result any
		if err := json.Unmarshal(resp.Result, &result); err == nil {
			out["result"] = result
		} else {
			out["result"] = string(resp.Result)
		}
	}
	return out
}

func requiredUint64Arg(args map[string]any, key string) (uint64, error) {
	raw, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("missing %s", key)
	}
	switch value := raw.(type) {
	case float64:
		if value < 1 || value > maxSafeJSONInteger || value != math.Trunc(value) {
			return 0, fmt.Errorf("%s must be a positive integer", key)
		}
		return uint64(value), nil
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return 0, fmt.Errorf("%s must be a positive integer", key)
		}
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed == 0 {
			return 0, fmt.Errorf("%s must be a positive integer", key)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
}

func requiredInt64Arg(args map[string]any, key string, minValue,
	maxValue int64) (int64, error) {

	raw, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("missing %s", key)
	}
	var value int64
	switch raw := raw.(type) {
	case float64:
		if raw != math.Trunc(raw) || raw < float64(minValue) ||
			raw > float64(maxValue) {

			return 0, fmt.Errorf("%s must be an integer between %d and %d",
				key, minValue, maxValue)
		}
		value = int64(raw)
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer between %d and %d",
				key, minValue, maxValue)
		}
		value = parsed
	default:
		return 0, fmt.Errorf("%s must be an integer between %d and %d",
			key, minValue, maxValue)
	}
	if value < minValue || value > maxValue {
		return 0, fmt.Errorf("%s must be an integer between %d and %d",
			key, minValue, maxValue)
	}
	return value, nil
}
