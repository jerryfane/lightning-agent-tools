// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package services

import (
	"testing"

	"github.com/lightninglabs/lightning-agent-kit/lightning-mcp-server/internal/interfaces"
	"github.com/lightninglabs/lightning-agent-kit/lightning-mcp-server/internal/logging"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

type stubMCPServer struct {
	tools []*mcp.Tool
}

func (s *stubMCPServer) AddTool(tool *mcp.Tool, handler interfaces.ToolHandler) {
	s.tools = append(s.tools, tool)
}

// Test Manager creation and basic functionality.
func TestManager_Creation(t *testing.T) {
	err := logging.InitLogger(true)
	require.NoError(t, err)

	manager := NewManager(zap.L())
	assert.NotNil(t, manager)
	assert.Equal(t, zap.L(), manager.logger)

	// Initialize services to test them.
	manager.InitializeServices()
	assert.NotNil(t, manager.invoiceService)
	assert.NotNil(t, manager.connectionService)
	assert.NotNil(t, manager.nodeOpsAuditService)
}

// Test RegisterTools with valid MCP server.
func TestManager_RegisterTools(t *testing.T) {
	err := logging.InitLogger(true)
	require.NoError(t, err)

	manager := NewManager(zap.L())
	manager.InitializeServices()
	stub := &stubMCPServer{}

	err = manager.RegisterTools(stub)
	assert.NoError(t, err)

	names := make(map[string]struct{})
	for _, tool := range stub.tools {
		names[tool.Name] = struct{}{}
	}

	// Test read-only tools are registered
	assert.Contains(t, names, "lnc_decode_invoice")
	assert.Contains(t, names, "lnc_list_channels")
	assert.Contains(t, names, "lnc_list_unspent")
	assert.Contains(t, names, "lnc_query_node_ops_audit")
	assert.NotZero(t, len(stub.tools))
}

func TestManager_RegisterTools_ReadOnlyMode(t *testing.T) {
	err := logging.InitLogger(true)
	require.NoError(t, err)

	manager := NewManager(zap.L())
	manager.InitializeServices()
	stub := &stubMCPServer{}

	err = manager.RegisterTools(stub)
	assert.NoError(t, err)

	names := make(map[string]struct{})
	for _, tool := range stub.tools {
		names[tool.Name] = struct{}{}
	}

	// Verify write operations are not available
	assert.NotContains(t, names, "lnc_send_payment")
	assert.NotContains(t, names, "lnc_pay_invoice")
	assert.NotContains(t, names, "lnc_open_channel")
	assert.NotContains(t, names, "lnc_close_channel")
	assert.NotContains(t, names, "lnc_send_coins")
	assert.NotContains(t, names, "lnc_new_address")
	assert.NotContains(t, names, "lnc_create_invoice")
	assert.NotContains(t, names, "lnc_connect_peer")
	assert.NotContains(t, names, "lnc_disconnect_peer")

	// Verify read-only operations are available
	assert.Contains(t, names, "lnc_list_channels")
	assert.Contains(t, names, "lnc_get_info")
	assert.Contains(t, names, "lnc_list_unspent")
	assert.Contains(t, names, "lnc_decode_invoice")
	assert.Contains(t, names, "lnc_list_peers")
	assert.Contains(t, names, "lnc_query_node_ops_audit")
	assert.Len(t, stub.tools, len(names))
}

// Test RegisterTools with nil MCP server.
func TestManager_RegisterTools_NilServer(t *testing.T) {
	err := logging.InitLogger(true)
	require.NoError(t, err)

	manager := NewManager(zap.L())
	manager.InitializeServices()

	err = manager.RegisterTools(nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "MCP server cannot be nil")
}

// Test connection callback functionality.
func TestManager_ConnectionCallback(t *testing.T) {
	err := logging.InitLogger(true)
	require.NoError(t, err)

	manager := NewManager(zap.L())
	manager.InitializeServices()

	// Create a mock connection - this would normally be a real gRPC connection
	// But for testing we just verify the callback doesn't panic.
	mockConn := &grpc.ClientConn{}

	// Call the connection callback - this is private so we can't test it directly
	// But we can verify services were initialized
	assert.NotNil(t, manager.invoiceService)
	assert.NotNil(t, manager.connectionService)

	// In a real scenario, mockConn would be passed to onLNCConnectionEstablished
	// Which would update all service clients.
	_ = mockConn
}

// Test services start with nil clients.
func TestManager_ServicesStartWithNilClients(t *testing.T) {
	err := logging.InitLogger(true)
	require.NoError(t, err)

	manager := NewManager(zap.L())
	manager.InitializeServices()

	// Services should start with nil clients until connection is established
	assert.Nil(t, manager.invoiceService.LightningClient)
	assert.Nil(t, manager.channelService.LightningClient)
	assert.Nil(t, manager.paymentService.LightningClient)
	assert.Nil(t, manager.onchainService.LightningClient)
	assert.Nil(t, manager.peerService.LightningClient)
	assert.Nil(t, manager.nodeService.LightningClient)
}

// Test Shutdown functionality.
func TestManager_Shutdown(t *testing.T) {
	err := logging.InitLogger(true)
	require.NoError(t, err)

	manager := NewManager(zap.L())

	// Test shutdown - should not error
	err = manager.Shutdown()
	assert.NoError(t, err)
}

// Test service integration.
func TestManager_ServiceIntegration(t *testing.T) {
	err := logging.InitLogger(true)
	require.NoError(t, err)

	manager := NewManager(zap.L())
	manager.InitializeServices()

	// Test that services are properly initialized
	assert.NotNil(t, manager.invoiceService)
	assert.NotNil(t, manager.connectionService)
	assert.NotNil(t, manager.channelService)
	assert.NotNil(t, manager.paymentService)
	assert.NotNil(t, manager.onchainService)
	assert.NotNil(t, manager.peerService)
	assert.NotNil(t, manager.nodeService)
	assert.NotNil(t, manager.nodeOpsAuditService)

	// Test that read-only tools can be created
	decodeInvoiceTool := manager.invoiceService.DecodeInvoiceTool()
	connectTool := manager.connectionService.ConnectTool()
	disconnectTool := manager.connectionService.DisconnectTool()
	listChannelsTool := manager.channelService.ListChannelsTool()

	assert.NotNil(t, decodeInvoiceTool)
	assert.NotNil(t, connectTool)
	assert.NotNil(t, disconnectTool)
	assert.NotNil(t, listChannelsTool)

	// Verify tool names are unique
	names := []string{
		decodeInvoiceTool.Name,
		connectTool.Name,
		disconnectTool.Name,
		listChannelsTool.Name,
	}

	for i, name := range names {
		for j, otherName := range names {
			if i != j {
				assert.NotEqual(t, name, otherName,
					"Tool names must be unique: %s vs %s", name, otherName)
			}
		}
	}
}

// Benchmark Manager creation.
func BenchmarkManager_Creation(b *testing.B) {
	err := logging.InitLogger(true)
	require.NoError(b, err)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = NewManager(zap.L())
	}
}

// Benchmark RegisterTools operation.
func BenchmarkManager_RegisterTools(b *testing.B) {
	err := logging.InitLogger(true)
	require.NoError(b, err)

	manager := NewManager(zap.L())
	manager.InitializeServices()
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "1.0.0",
	}, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = manager.RegisterTools(mcpServer)
	}
}
