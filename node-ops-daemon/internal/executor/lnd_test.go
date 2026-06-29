// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNewLNDExecutorRejectsNonRegtest(t *testing.T) {
	_, err := NewLNDExecutor(context.Background(), LNDConfig{
		RPCAddress:      "127.0.0.1:10009",
		MacaroonPath:    "/tmp/node-ops.macaroon",
		TLSCertPath:     "/tmp/tls.cert",
		RequiredNetwork: "mainnet",
		DialTimeout:     time.Second,
		RequestTimeout:  time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "regtest") {
		t.Fatalf("expected regtest-only error, got %v", err)
	}
}

func TestParseChannelPoint(t *testing.T) {
	const txid = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cp, err := parseChannelPoint(txid + ":7")
	if err != nil {
		t.Fatalf("parseChannelPoint: %v", err)
	}
	if cp.GetFundingTxidStr() != txid || cp.GetOutputIndex() != 7 {
		t.Fatalf("unexpected channel point: %+v", cp)
	}
}

func TestParseChannelPointRejectsInvalid(t *testing.T) {
	for _, value := range []string{"", "txid", "txid:not-int", ":1"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseChannelPoint(value); err == nil {
				t.Fatalf("expected error for %q", value)
			}
		})
	}
}
