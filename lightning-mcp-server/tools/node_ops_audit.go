// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultAuditLimit       = 50
	maxAuditLimit           = 500
	maxAuditOffset          = 1_000_000
	defaultNodeOpsDialDelay = 2 * time.Second
)

type nodeOpsDaemonCaller func(context.Context, string, time.Duration, string, any) (*nodeOpsDaemonResponse, error)

// NodeOpsAuditService queries the node-ops-daemon audit ledger over its local
// Unix socket. It does not require an LNC connection and never writes to LND.
type NodeOpsAuditService struct {
	DaemonSocket string
	DialTimeout  time.Duration
	callDaemon   nodeOpsDaemonCaller
}

type nodeOpsDaemonResponse struct {
	Status    string          `json:"status"`
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Reason    string          `json:"reason,omitempty"`
}

// DefaultNodeOpsDaemonSocket returns the configured node-ops-daemon socket path.
func DefaultNodeOpsDaemonSocket() string {
	if value := strings.TrimSpace(os.Getenv("NODE_OPS_DAEMON_SOCKET")); value != "" {
		return expandHomePath(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".node-ops", "daemon.sock")
	}
	return filepath.Join(home, ".node-ops", "daemon.sock")
}

// NewNodeOpsAuditService creates a service for read-only audit ledger queries.
func NewNodeOpsAuditService(socketPath string) *NodeOpsAuditService {
	return &NodeOpsAuditService{
		DaemonSocket: socketPath,
		DialTimeout:  defaultNodeOpsDialDelay,
		callDaemon:   callNodeOpsDaemon,
	}
}

// QueryAuditLedgerTool returns the MCP tool definition.
func (s *NodeOpsAuditService) QueryAuditLedgerTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "lnc_query_node_ops_audit",
		Description: "Query the node-ops-daemon append-only audit ledger. " +
			"Read-only - no Lightning node writes are made.",
		InputSchema: ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"request_id": map[string]any{
					"type":        "string",
					"description": "Optional exact request_id filter.",
				},
				"action": map[string]any{
					"type":        "string",
					"description": "Optional action filter, such as execute_fee_set.",
				},
				"status": map[string]any{
					"type":        "string",
					"description": "Optional status filter, such as pending, executed, rejected, failed, or ok.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum entries to return (default 50, max 500).",
					"minimum":     1,
					"maximum":     maxAuditLimit,
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Entries to skip after filtering (default 0).",
					"minimum":     0,
				},
				"newest_first": map[string]any{
					"type":        "boolean",
					"description": "Return newest entries first (default true).",
				},
			},
		},
	}
}

// HandleQueryAuditLedger handles the read-only audit query request.
func (s *NodeOpsAuditService) HandleQueryAuditLedger(
	ctx context.Context,
	request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {

	args := requestArguments(request)
	params := map[string]any{
		"limit":        boundedIntArg(args, "limit", defaultAuditLimit, 1, maxAuditLimit),
		"offset":       boundedIntArg(args, "offset", 0, 0, maxAuditOffset),
		"newest_first": true,
	}
	if requestID, ok := stringArg(args, "request_id"); ok {
		params["request_id"] = requestID
	}
	if action, ok := stringArg(args, "action"); ok {
		params["action"] = action
	}
	if status, ok := stringArg(args, "status"); ok {
		params["status"] = status
	}
	if newestFirst, ok := args["newest_first"].(bool); ok {
		params["newest_first"] = newestFirst
	}

	call := s.callDaemon
	if call == nil {
		call = callNodeOpsDaemon
	}
	resp, err := call(
		ctx, s.DaemonSocket, s.DialTimeout, "query_audit_log", params,
	)
	if err != nil {
		return newToolResultError(
			"Failed to query node-ops audit ledger: " + err.Error()), nil
	}
	if resp.Status != "ok" {
		reason := resp.Reason
		if reason == "" {
			reason = "node-ops daemon returned status " + resp.Status
		}
		return newToolResultError(reason), nil
	}
	if len(resp.Result) == 0 {
		return newToolResultJSON(map[string]any{}), nil
	}
	return newToolResultText(string(resp.Result)), nil
}

func callNodeOpsDaemon(ctx context.Context, socketPath string, timeout time.Duration,
	action string, params any) (*nodeOpsDaemonResponse, error) {

	if strings.TrimSpace(socketPath) == "" {
		return nil, fmt.Errorf("node-ops daemon socket path is empty")
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	body, err := json.Marshal(map[string]any{
		"action": action,
		"params": params,
	})
	if err != nil {
		return nil, fmt.Errorf("encode daemon request: %w", err)
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return nil, err
	}
	if _, err := conn.Write(body); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(lenBuf[:])
	respBody := make([]byte, size)
	if _, err := io.ReadFull(conn, respBody); err != nil {
		return nil, err
	}
	var resp nodeOpsDaemonResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	return &resp, nil
}

func stringArg(args map[string]any, key string) (string, bool) {
	value, ok := args[key].(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

func expandHomePath(path string) string {
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
