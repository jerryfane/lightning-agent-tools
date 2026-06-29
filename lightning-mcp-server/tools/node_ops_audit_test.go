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

func makeNodeOpsAuditRequest(args map[string]any) *mcp.CallToolRequest {
	b, _ := json.Marshal(args)
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{
			Arguments: json.RawMessage(b),
		},
	}
}

func TestNodeOpsAuditService_QueryAuditLedgerTool(t *testing.T) {
	svc := NewNodeOpsAuditService("/tmp/node-ops.sock")
	tool := svc.QueryAuditLedgerTool()

	assert.Equal(t, "lnc_query_node_ops_audit", tool.Name)
	assert.Contains(t, tool.Description, "Read-only")
	schema := requireToolSchema(t, tool.InputSchema)
	assert.Equal(t, "object", schema.Type)
	assert.Contains(t, schema.Properties, "request_id")
	assert.Contains(t, schema.Properties, "action")
	assert.Contains(t, schema.Properties, "status")
	assert.Contains(t, schema.Properties, "limit")
	assert.Contains(t, schema.Properties, "offset")
	assert.Contains(t, schema.Properties, "newest_first")
}

func TestNodeOpsAuditService_HandleQueryAuditLedger(t *testing.T) {
	svc := NewNodeOpsAuditService("/tmp/node-ops.sock")
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
			Status: "ok",
			Result: json.RawMessage(
				`{"count":1,"entries":[{"id":1,"action":"execute_fee_set"}]}`),
		}, nil
	}

	result, err := svc.HandleQueryAuditLedger(
		context.Background(),
		makeNodeOpsAuditRequest(map[string]any{
			"request_id":   " req-1 ",
			"action":       " execute_fee_set ",
			"status":       " rejected ",
			"limit":        100,
			"offset":       2,
			"newest_first": false,
		}),
	)
	require.NoError(t, err)
	require.False(t, result.IsError)
	text := result.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, `"count":1`)
	assert.Equal(t, "/tmp/node-ops.sock", gotSocket)
	assert.Equal(t, defaultNodeOpsDialDelay, gotTimeout)
	assert.Equal(t, "query_audit_log", gotAction)
	assert.Equal(t, "req-1", gotParams["request_id"])
	assert.Equal(t, "execute_fee_set", gotParams["action"])
	assert.Equal(t, "rejected", gotParams["status"])
	assert.Equal(t, 100, gotParams["limit"])
	assert.Equal(t, 2, gotParams["offset"])
	assert.Equal(t, false, gotParams["newest_first"])
}

func TestNodeOpsAuditService_HandleQueryAuditLedgerErrors(t *testing.T) {
	t.Run("daemon call failure", func(t *testing.T) {
		svc := NewNodeOpsAuditService("/tmp/node-ops.sock")
		svc.callDaemon = func(context.Context, string, time.Duration, string, any) (*nodeOpsDaemonResponse, error) {
			return nil, errors.New("dial failed")
		}

		result, err := svc.HandleQueryAuditLedger(
			context.Background(), makeNodeOpsAuditRequest(nil),
		)
		require.NoError(t, err)
		require.True(t, result.IsError)
		text := result.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "dial failed")
	})

	t.Run("daemon error response", func(t *testing.T) {
		svc := NewNodeOpsAuditService("/tmp/node-ops.sock")
		svc.callDaemon = func(context.Context, string, time.Duration, string, any) (*nodeOpsDaemonResponse, error) {
			return &nodeOpsDaemonResponse{
				Status: "error",
				Reason: "audit ledger unavailable",
			}, nil
		}

		result, err := svc.HandleQueryAuditLedger(
			context.Background(), makeNodeOpsAuditRequest(nil),
		)
		require.NoError(t, err)
		require.True(t, result.IsError)
		text := result.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "audit ledger unavailable")
	})
}
