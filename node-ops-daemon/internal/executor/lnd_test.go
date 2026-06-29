// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
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

func TestRebalanceFeeLimitSat(t *testing.T) {
	tests := []struct {
		name      string
		amountSat int64
		feePPM    int64
		want      int64
	}{
		{name: "zero fee", amountSat: 1000, feePPM: 0, want: 0},
		{name: "round up", amountSat: 999, feePPM: 1, want: 1},
		{name: "exact", amountSat: 1_000_000, feePPM: 500, want: 500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rebalanceFeeLimitSat(tc.amountSat, tc.feePPM)
			if err != nil {
				t.Fatalf("rebalanceFeeLimitSat: %v", err)
			}
			if got != tc.want {
				t.Fatalf("fee limit = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestVerifyRebalanceRoutePinsBoundaryChannels(t *testing.T) {
	req := RebalanceRequest{
		OutgoingChanID: 10,
		IncomingChanID: 20,
		AmountSat:      1000,
		MaxFeePpm:      500,
	}
	route := &lnrpc.Route{Hops: []*lnrpc.Hop{
		{ChanId: 10},
		{ChanId: 11},
		{ChanId: 20},
	}}
	if err := verifyRebalanceRoute(route, req); err != nil {
		t.Fatalf("verifyRebalanceRoute: %v", err)
	}

	route.Hops[0].ChanId = 99
	if err := verifyRebalanceRoute(route, req); err == nil ||
		!strings.Contains(err.Error(), "outgoing_chan_id") {

		t.Fatalf("expected outgoing channel rejection, got %v", err)
	}
}

func TestFeePPMFromMsat(t *testing.T) {
	got, err := feePPMFromMsat(1000, 1500)
	if err != nil {
		t.Fatalf("feePPMFromMsat: %v", err)
	}
	if got != 1500 {
		t.Fatalf("fee ppm = %d, want 1500", got)
	}

	got, err = feePPMFromMsat(3, 1)
	if err != nil {
		t.Fatalf("feePPMFromMsat: %v", err)
	}
	if got != 334 {
		t.Fatalf("fee ppm should round up, got %d", got)
	}
}
