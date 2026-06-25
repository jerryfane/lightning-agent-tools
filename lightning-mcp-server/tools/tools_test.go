// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package tools

import (
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func requireToolSchema(t *testing.T, toolSchema any) ToolInputSchema {
	t.Helper()

	schema, ok := toolSchema.(ToolInputSchema)
	require.True(t, ok)
	return schema
}

// Test InvoiceService basic functionality.
func TestInvoiceService_ToolCreation(t *testing.T) {
	service := NewInvoiceService(nil)

	t.Run("list_invoices_tool", func(t *testing.T) {
		tool := service.ListInvoicesTool()
		assert.Equal(t, "lnc_list_invoices", tool.Name)
		assert.Contains(t, tool.Description, "List invoices created by this Lightning node")
		schema := requireToolSchema(t, tool.InputSchema)
		assert.Equal(t, "object", schema.Type)

		// Check optional fields exist.
		props := schema.Properties
		assert.Contains(t, props, "pending_only")
		assert.Contains(t, props, "index_offset")

		// Verify pending_only field structure.
		pendingField, ok := props["pending_only"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "boolean", pendingField["type"])
	})

	t.Run("decode_invoice_tool", func(t *testing.T) {
		tool := service.DecodeInvoiceTool()
		assert.Equal(t, "lnc_decode_invoice", tool.Name)
		assert.Contains(t, tool.Description, "Decode a BOLT11 Lightning invoice")
		schema := requireToolSchema(t, tool.InputSchema)
		assert.Equal(t, "object", schema.Type)

		// Check required fields.
		props := schema.Properties
		assert.Contains(t, props, "invoice")

		// Verify invoice field structure.
		invoiceField, ok := props["invoice"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "string", invoiceField["type"])
	})
}

func TestInvoiceService_ServiceManagement(t *testing.T) {
	// Test service creation.
	service := NewInvoiceService(nil)
	assert.NotNil(t, service)
	assert.Nil(t, service.LightningClient)

	// Test service with client update.
	service.LightningClient = nil // Simulate setting client later.
	assert.Nil(t, service.LightningClient)
}

// Test ConnectionService basic functionality.
func TestConnectionService_ToolCreation(t *testing.T) {
	callback := func(conn *grpc.ClientConn) {}
	service := NewConnectionService(callback)

	t.Run("connect_tool", func(t *testing.T) {
		tool := service.ConnectTool()
		assert.Equal(t, "lnc_connect", tool.Name)
		assert.Contains(t, tool.Description, "Connect to a Lightning node")
		schema := requireToolSchema(t, tool.InputSchema)
		assert.Equal(t, "object", schema.Type)

		// Check required fields.
		props := schema.Properties
		assert.Contains(t, props, "pairingPhrase")

		// Verify pairingPhrase field structure.
		pairingField, ok := props["pairingPhrase"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "string", pairingField["type"])

		// Check optional fields.
		assert.Contains(t, props, "mailbox")
		assert.Contains(t, props, "devMode")
		assert.Contains(t, props, "password")

		// Verify required fields list contains pairingPhrase.
		require.Contains(t, schema.Required, "pairingPhrase")
	})

	t.Run("disconnect_tool", func(t *testing.T) {
		tool := service.DisconnectTool()
		assert.Equal(t, "lnc_disconnect", tool.Name)
		assert.Contains(t, tool.Description,
			"Disconnect from the Lightning node")
		schema := requireToolSchema(t, tool.InputSchema)
		assert.Equal(t, "object", schema.Type)

		// Disconnect tool should have no required parameters.
		assert.Equal(t, 0, len(schema.Required))
	})
}

func TestConnectionService_ServiceManagement(t *testing.T) {
	callback := func(conn *grpc.ClientConn) {}
	service := NewConnectionService(callback)
	assert.NotNil(t, service)

	// Test that we can create tools.
	connectTool := service.ConnectTool()
	disconnectTool := service.DisconnectTool()

	assert.NotNil(t, connectTool)
	assert.NotNil(t, disconnectTool)
	assert.NotEqual(t, connectTool.Name, disconnectTool.Name)
}

// Test helper functions and utilities.
func TestPairingPhraseValidation(t *testing.T) {
	// Test the word counting logic used in connection service.
	tests := []struct {
		name          string
		phrase        string
		expectValid   bool
		expectedWords int
	}{
		{
			name:          "exactly_10_words",
			phrase:        "one two three four five six seven eight nine ten",
			expectValid:   true,
			expectedWords: 10,
		},
		{
			name:          "9_words",
			phrase:        "one two three four five six seven eight nine",
			expectValid:   false,
			expectedWords: 9,
		},
		{
			name: "11_words",
			phrase: "one two three four five six seven eight nine ten " +
				"eleven",
			expectValid:   false,
			expectedWords: 11,
		},
		{
			name:          "extra_spaces_handled",
			phrase:        "one  two   three four five six seven eight nine ten",
			expectValid:   true, // strings.Fields handles extra spaces.
			expectedWords: 10,
		},
		{
			name:          "leading_trailing_spaces",
			phrase:        " one two three four five six seven eight nine ten ",
			expectValid:   true, // strings.Fields trims spaces.
			expectedWords: 10,
		},
		{
			name:          "empty_string",
			phrase:        "",
			expectValid:   false,
			expectedWords: 0,
		},
		{
			name:          "only_spaces",
			phrase:        "   ",
			expectValid:   false,
			expectedWords: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			words := strings.Fields(tt.phrase)
			actualWordCount := len(words)

			assert.Equal(t, tt.expectedWords, actualWordCount)

			isValid := actualWordCount == 10
			assert.Equal(t, tt.expectValid, isValid)
		})
	}
}

// Test service integration.
func TestServiceIntegration(t *testing.T) {
	t.Run("invoice_service_complete", func(t *testing.T) {
		service := NewInvoiceService(nil)

		// Verify tools are created correctly.
		listTool := service.ListInvoicesTool()
		decodeTool := service.DecodeInvoiceTool()

		assert.NotEmpty(t, listTool.Name)
		assert.NotEmpty(t, decodeTool.Name)
		assert.NotEqual(t, listTool.Name, decodeTool.Name)

		// Test service state management.
		assert.Nil(t, service.LightningClient)
	})

	t.Run("connection_service_complete", func(t *testing.T) {
		callback := func(conn *grpc.ClientConn) {}
		service := NewConnectionService(callback)

		connectTool := service.ConnectTool()
		disconnectTool := service.DisconnectTool()

		assert.NotNil(t, connectTool)
		assert.NotNil(t, disconnectTool)
		assert.NotEqual(t, connectTool.Name, disconnectTool.Name)

		// Test tools have proper structure.
		assert.NotEmpty(t, connectTool.Name)
		assert.NotEmpty(t, connectTool.Description)
		assert.NotNil(t, connectTool.InputSchema)

		assert.NotEmpty(t, disconnectTool.Name)
		assert.NotEmpty(t, disconnectTool.Description)
		assert.NotNil(t, disconnectTool.InputSchema)
	})
}

// Test helper to create test invoice.
func createTestInvoice(amount int64, memo string) *lnrpc.Invoice {
	return &lnrpc.Invoice{
		ValueMsat: amount * 1000,
		Memo:      memo,
		Expiry:    3600,
	}
}

func TestChannelActionsService_ToolCreation(t *testing.T) {
	service := NewChannelActionsService(nil)

	t.Run("propose_channel_actions_tool", func(t *testing.T) {
		tool := service.ProposeChannelActionsTool()
		assert.Equal(t, "lnc_propose_channel_actions", tool.Name)
		assert.Contains(t, tool.Description, "Read-only")
		schema := requireToolSchema(t, tool.InputSchema)
		assert.Equal(t, "object", schema.Type)

		props := schema.Properties
		assert.Contains(t, props, "lookback_days")
		assert.Contains(t, props, "max_close_candidates")
		assert.Contains(t, props, "max_open_candidates")

		lookbackField, ok := props["lookback_days"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "integer", lookbackField["type"])
	})
}

func TestChannelActionsService_NilClient(t *testing.T) {
	service := NewChannelActionsService(nil)
	assert.NotNil(t, service)
	assert.Nil(t, service.LightningClient)
}

func TestCreateTestInvoice(t *testing.T) {
	invoice := createTestInvoice(1000, "test memo")
	assert.Equal(t, int64(1000000), invoice.ValueMsat)
	assert.Equal(t, "test memo", invoice.Memo)
	assert.Equal(t, int64(3600), invoice.Expiry)
}

// Benchmark tests for performance.
func BenchmarkInvoiceService_ListInvoicesTool(b *testing.B) {
	service := NewInvoiceService(nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = service.ListInvoicesTool()
	}
}

func BenchmarkConnectionService_ConnectTool(b *testing.B) {
	callback := func(conn *grpc.ClientConn) {}
	service := NewConnectionService(callback)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = service.ConnectTool()
	}
}

func BenchmarkPairingPhraseValidation(b *testing.B) {
	phrases := []string{
		"one two three four five six seven eight nine ten",
		"one two three",
		"one two three four five six seven eight nine ten eleven twelve",
		"",
		"   ",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, phrase := range phrases {
			words := strings.Fields(phrase)
			_ = len(words) == 10
		}
	}
}
