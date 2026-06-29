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

func makeNodeOpsFeeSetRequest(args map[string]any) *mcp.CallToolRequest {
	b, _ := json.Marshal(args)
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Arguments: json.RawMessage(b),
		},
	}
}

func TestNodeOpsFeeSetService_ExecuteFeeSetTool(t *testing.T) {
	svc := NewNodeOpsFeeSetService("/tmp/node-ops.sock")
	tool := svc.ExecuteFeeSetTool()

	assert.Equal(t, "lnc_execute_fee_set", tool.Name)
	assert.Contains(t, tool.Description, "node-ops-daemon")
	schema := requireToolSchema(t, tool.InputSchema)
	assert.Equal(t, "object", schema.Type)
	assert.ElementsMatch(t,
		[]string{"chan_id", "base_msat", "fee_ppm"}, schema.Required)
	assert.Contains(t, schema.Properties, "chan_id")
	assert.Contains(t, schema.Properties, "base_msat")
	assert.Contains(t, schema.Properties, "fee_ppm")
}

func TestNodeOpsFeeSetService_HandleExecuteFeeSet(t *testing.T) {
	svc := NewNodeOpsFeeSetService("/tmp/node-ops.sock")
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
			RequestID: "req-123",
			Result: json.RawMessage(
				`{"queued":true,"request_id":"req-123"}`),
		}, nil
	}

	result, err := svc.HandleExecuteFeeSet(
		context.Background(),
		makeNodeOpsFeeSetRequest(map[string]any{
			"chan_id":   "42",
			"base_msat": 1000,
			"fee_ppm":   110,
		}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)
	text := result.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, `"status": "pending"`)
	assert.Contains(t, text, `"request_id": "req-123"`)
	assert.Contains(t, text, `"queued": true`)
	assert.Equal(t, "/tmp/node-ops.sock", gotSocket)
	assert.Equal(t, defaultNodeOpsDialDelay, gotTimeout)
	assert.Equal(t, "execute_fee_set", gotAction)
	assert.Equal(t, uint64(42), gotParams["chan_id"])
	assert.Equal(t, int64(1000), gotParams["base_msat"])
	assert.Equal(t, int64(110), gotParams["fee_ppm"])
}

func TestNodeOpsFeeSetService_HandleExecuteFeeSetErrors(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		svc := NewNodeOpsFeeSetService("/tmp/node-ops.sock")
		called := false
		svc.callDaemon = func(context.Context, string, time.Duration, string, any) (*nodeOpsDaemonResponse, error) {
			called = true
			return nil, nil
		}

		result, err := svc.HandleExecuteFeeSet(
			context.Background(),
			makeNodeOpsFeeSetRequest(map[string]any{
				"chan_id": 0, "base_msat": 1000, "fee_ppm": 110,
			}),
		)
		require.NoError(t, err)
		require.True(t, result.IsError)
		assert.False(t, called)
	})

	t.Run("daemon call failure", func(t *testing.T) {
		svc := NewNodeOpsFeeSetService("/tmp/node-ops.sock")
		svc.callDaemon = func(context.Context, string, time.Duration, string, any) (*nodeOpsDaemonResponse, error) {
			return nil, errors.New("dial failed")
		}

		result, err := svc.HandleExecuteFeeSet(
			context.Background(),
			makeNodeOpsFeeSetRequest(map[string]any{
				"chan_id": 42, "base_msat": 1000, "fee_ppm": 110,
			}),
		)
		require.NoError(t, err)
		require.True(t, result.IsError)
		text := result.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "dial failed")
	})

	t.Run("daemon rejection", func(t *testing.T) {
		svc := NewNodeOpsFeeSetService("/tmp/node-ops.sock")
		svc.callDaemon = func(context.Context, string, time.Duration, string, any) (*nodeOpsDaemonResponse, error) {
			return &nodeOpsDaemonResponse{
				Status: "error",
				Reason: "fee delta 150 ppm exceeds max_fee_ppm_delta 100",
			}, nil
		}

		result, err := svc.HandleExecuteFeeSet(
			context.Background(),
			makeNodeOpsFeeSetRequest(map[string]any{
				"chan_id": 42, "base_msat": 1000, "fee_ppm": 250,
			}),
		)
		require.NoError(t, err)
		require.True(t, result.IsError)
		text := result.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "max_fee_ppm_delta")
	})
}
