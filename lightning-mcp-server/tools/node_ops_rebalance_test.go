// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeNodeOpsRebalanceRequest(args map[string]any) *mcp.CallToolRequest {
	b, _ := json.Marshal(args)
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Arguments: json.RawMessage(b),
		},
	}
}

func TestNodeOpsRebalanceService_ExecuteRebalanceTool(t *testing.T) {
	svc := NewNodeOpsRebalanceService("/tmp/node-ops.sock")
	tool := svc.ExecuteRebalanceTool()

	assert.Equal(t, "lnc_execute_rebalance", tool.Name)
	assert.Contains(t, tool.Description, "node-ops-daemon")
	schema := requireToolSchema(t, tool.InputSchema)
	assert.Equal(t, "object", schema.Type)
	assert.ElementsMatch(t,
		[]string{"outgoing_chan_id", "incoming_chan_id", "amount_sat", "max_fee_ppm"},
		schema.Required)
	assert.Contains(t, schema.Properties, "outgoing_chan_id")
	assert.Contains(t, schema.Properties, "incoming_chan_id")
	assert.Contains(t, schema.Properties, "amount_sat")
	assert.Contains(t, schema.Properties, "max_fee_ppm")
}

func TestNodeOpsRebalanceService_HandleExecuteRebalance(t *testing.T) {
	svc := NewNodeOpsRebalanceService("/tmp/node-ops.sock")
	var gotSocket string
	var gotTimeout time.Duration
	var gotAction string
	var gotParams map[string]any
	svc.callDaemon = func(_ context.Context, socket string, timeout time.Duration,
		action string, params any) (*nodeOpsDaemonResponse, error) {

		gotSocket = socket
		gotTimeout = timeout
		gotAction = action
		gotParams = params.(map[string]any)
		return &nodeOpsDaemonResponse{
			Status:    "pending",
			RequestID: "req-456",
			Result: json.RawMessage(
				`{"queued":true,"request_id":"req-456"}`),
		}, nil
	}

	result, err := svc.HandleExecuteRebalance(
		context.Background(),
		makeNodeOpsRebalanceRequest(map[string]any{
			"outgoing_chan_id": "42",
			"incoming_chan_id": "43",
			"amount_sat":       100000,
			"max_fee_ppm":      400,
		}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)
	text := result.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, `"status": "pending"`)
	assert.Contains(t, text, `"request_id": "req-456"`)
	assert.Contains(t, text, `"queued": true`)
	assert.Equal(t, "/tmp/node-ops.sock", gotSocket)
	assert.Equal(t, defaultNodeOpsDialDelay, gotTimeout)
	assert.Equal(t, "execute_rebalance", gotAction)
	assert.Equal(t, uint64(42), gotParams["outgoing_chan_id"])
	assert.Equal(t, uint64(43), gotParams["incoming_chan_id"])
	assert.Equal(t, int64(100000), gotParams["amount_sat"])
	assert.Equal(t, int64(400), gotParams["max_fee_ppm"])
}

func TestNodeOpsRebalanceService_HandleExecuteRebalanceErrors(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		svc := NewNodeOpsRebalanceService("/tmp/node-ops.sock")
		called := false
		svc.callDaemon = func(context.Context, string, time.Duration, string, any) (*nodeOpsDaemonResponse, error) {
			called = true
			return nil, nil
		}

		result, err := svc.HandleExecuteRebalance(
			context.Background(),
			makeNodeOpsRebalanceRequest(map[string]any{
				"outgoing_chan_id": 42,
				"incoming_chan_id": 43,
				"amount_sat":       0,
				"max_fee_ppm":      400,
			}),
		)
		require.NoError(t, err)
		require.True(t, result.IsError)
		assert.False(t, called)
	})

	t.Run("daemon call failure", func(t *testing.T) {
		svc := NewNodeOpsRebalanceService("/tmp/node-ops.sock")
		svc.callDaemon = func(context.Context, string, time.Duration, string, any) (*nodeOpsDaemonResponse, error) {
			return nil, errors.New("dial failed")
		}

		result, err := svc.HandleExecuteRebalance(
			context.Background(),
			makeNodeOpsRebalanceRequest(map[string]any{
				"outgoing_chan_id": 42,
				"incoming_chan_id": 43,
				"amount_sat":       1000,
				"max_fee_ppm":      400,
			}),
		)
		require.NoError(t, err)
		require.True(t, result.IsError)
		text := result.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "dial failed")
	})

	t.Run("daemon rejection", func(t *testing.T) {
		svc := NewNodeOpsRebalanceService("/tmp/node-ops.sock")
		svc.callDaemon = func(context.Context, string, time.Duration, string, any) (*nodeOpsDaemonResponse, error) {
			return &nodeOpsDaemonResponse{
				Status: "error",
				Reason: "fee_ppm 600 exceeds rebalance_max_fee_ppm 500",
			}, nil
		}

		result, err := svc.HandleExecuteRebalance(
			context.Background(),
			makeNodeOpsRebalanceRequest(map[string]any{
				"outgoing_chan_id": 42,
				"incoming_chan_id": 43,
				"amount_sat":       1000,
				"max_fee_ppm":      600,
			}),
		)
		require.NoError(t, err)
		require.True(t, result.IsError)
		text := result.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "rebalance_max_fee_ppm")
	})
}
