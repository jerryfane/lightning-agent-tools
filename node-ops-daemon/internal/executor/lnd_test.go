// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
)

type mockLNDHealthClient struct {
	lnrpc.LightningClient
	info       *lnrpc.GetInfoResponse
	infoErr    error
	pending    *lnrpc.PendingChannelsResponse
	pendingErr error
	peers      *lnrpc.ListPeersResponse
	peersErr   error
}

func (m *mockLNDHealthClient) GetInfo(context.Context,
	*lnrpc.GetInfoRequest, ...grpc.CallOption) (*lnrpc.GetInfoResponse, error) {

	return m.info, m.infoErr
}

func (m *mockLNDHealthClient) PendingChannels(context.Context,
	*lnrpc.PendingChannelsRequest,
	...grpc.CallOption) (*lnrpc.PendingChannelsResponse, error) {

	return m.pending, m.pendingErr
}

func (m *mockLNDHealthClient) ListPeers(context.Context,
	*lnrpc.ListPeersRequest, ...grpc.CallOption) (*lnrpc.ListPeersResponse, error) {

	return m.peers, m.peersErr
}

func newTestHealthExecutor(client lnrpc.LightningClient) *LNDExecutor {
	return &LNDExecutor{
		client:          client,
		requiredNetwork: "regtest",
		requestTimeout:  time.Second,
	}
}

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

func TestLNDExecutorNodeHealthHealthy(t *testing.T) {
	exec := newTestHealthExecutor(&mockLNDHealthClient{
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: "node",
			Alias:          "test-node",
			SyncedToChain:  true,
			SyncedToGraph:  true,
			BlockHeight:    42,
			Chains: []*lnrpc.Chain{{
				Chain:   "bitcoin",
				Network: "regtest",
			}},
		},
		pending: &lnrpc.PendingChannelsResponse{},
		peers:   &lnrpc.ListPeersResponse{},
	})

	got, err := exec.NodeHealth(context.Background())
	if err != nil {
		t.Fatalf("NodeHealth: %v", err)
	}
	if got.OverallStatus != "healthy" || got.AlertCount != 0 ||
		got.CriticalCount != 0 || got.WarningCount != 0 {

		t.Fatalf("unexpected healthy snapshot: %+v", got)
	}
	if got.NodeID != "node" || got.Alias != "test-node" ||
		!got.SyncedToChain || !got.SyncedToGraph || got.BlockHeight != 42 {

		t.Fatalf("unexpected node metadata: %+v", got)
	}
}

func TestLNDExecutorNodeHealthPrioritizesCriticalAlerts(t *testing.T) {
	exec := newTestHealthExecutor(&mockLNDHealthClient{
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: "node",
			Alias:          "test-node",
			SyncedToChain:  true,
			SyncedToGraph:  false,
			BlockHeight:    42,
			Chains: []*lnrpc.Chain{{
				Chain:   "bitcoin",
				Network: "regtest",
			}},
		},
		pending: &lnrpc.PendingChannelsResponse{
			PendingForceClosingChannels: []*lnrpc.PendingChannelsResponse_ForceClosedChannel{
				{
					Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
						ChannelPoint:  "force:0",
						RemoteNodePub: "remote-force",
					},
					LimboBalance:      10_000,
					BlocksTilMaturity: 3,
					ClosingTxid:       "close-tx",
				},
			},
			WaitingCloseChannels: []*lnrpc.PendingChannelsResponse_WaitingCloseChannel{
				{
					Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
						ChannelPoint:  "waiting:1",
						RemoteNodePub: "remote-waiting",
					},
					LimboBalance: 5_000,
				},
			},
		},
		peers: &lnrpc.ListPeersResponse{
			Peers: []*lnrpc.Peer{{
				PubKey:    "peer",
				Address:   "127.0.0.1:9735",
				FlapCount: 10,
			}},
		},
	})

	got, err := exec.NodeHealth(context.Background())
	if err != nil {
		t.Fatalf("NodeHealth: %v", err)
	}
	if got.OverallStatus != "critical" || got.AlertCount != 4 ||
		got.CriticalCount != 1 || got.WarningCount != 3 {

		t.Fatalf("unexpected alert counts: %+v", got)
	}
	if got.Alerts[0].ID != "channel:force-close:force:0" ||
		got.Alerts[0].Severity != "critical" {

		t.Fatalf("critical alert not first: %+v", got.Alerts)
	}
	wantIDs := []string{
		"channel:force-close:force:0",
		"sync:graph",
		"channel:waiting-close:waiting:1",
		"peer:flap:peer",
	}
	for i, want := range wantIDs {
		if got.Alerts[i].ID != want {
			t.Fatalf("alert %d id = %q, want %q", i, got.Alerts[i].ID, want)
		}
	}
	if got.Alerts[0].Details["closing_txid"] != "close-tx" {
		t.Fatalf("force-close details missing: %+v", got.Alerts[0].Details)
	}
}

func TestLNDExecutorNodeHealthClassifiesForceCloseWaitingClose(t *testing.T) {
	exec := newTestHealthExecutor(&mockLNDHealthClient{
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: "node",
			Alias:          "test-node",
			SyncedToChain:  true,
			SyncedToGraph:  true,
			Chains: []*lnrpc.Chain{{
				Chain:   "bitcoin",
				Network: "regtest",
			}},
		},
		pending: &lnrpc.PendingChannelsResponse{
			WaitingCloseChannels: []*lnrpc.PendingChannelsResponse_WaitingCloseChannel{
				{
					Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
						ChannelPoint:    "waiting:1",
						RemoteNodePub:   "remote-waiting",
						ChanStatusFlags: "ChanStatusBorked|ChanStatusCommitBroadcasted",
					},
					LimboBalance: 5000,
					Commitments:  &lnrpc.PendingChannelsResponse_Commitments{},
					ClosingTxid:  "force-close-tx",
				},
			},
		},
		peers: &lnrpc.ListPeersResponse{},
	})

	got, err := exec.NodeHealth(context.Background())
	if err != nil {
		t.Fatalf("NodeHealth: %v", err)
	}
	if got.OverallStatus != "critical" || got.CriticalCount != 1 ||
		got.WarningCount != 0 || got.AlertCount != 1 {

		t.Fatalf("unexpected force-close snapshot: %+v", got)
	}
	alert := got.Alerts[0]
	if alert.ID != "channel:force-close:waiting:1" ||
		alert.Severity != "critical" ||
		alert.Message != "Force-closing channel detected" {

		t.Fatalf("unexpected force-close alert: %+v", alert)
	}
	if alert.Details["closing_txid"] != "force-close-tx" {
		t.Fatalf("closing tx detail missing: %+v", alert.Details)
	}
}

func TestLNDExecutorNodeHealthKeepsOrdinaryWaitingCloseWarning(t *testing.T) {
	exec := newTestHealthExecutor(&mockLNDHealthClient{
		info: &lnrpc.GetInfoResponse{
			IdentityPubkey: "node",
			Alias:          "test-node",
			SyncedToChain:  true,
			SyncedToGraph:  true,
			Chains: []*lnrpc.Chain{{
				Chain:   "bitcoin",
				Network: "regtest",
			}},
		},
		pending: &lnrpc.PendingChannelsResponse{
			WaitingCloseChannels: []*lnrpc.PendingChannelsResponse_WaitingCloseChannel{
				{
					Channel: &lnrpc.PendingChannelsResponse_PendingChannel{
						ChannelPoint:  "waiting:1",
						RemoteNodePub: "remote-waiting",
					},
					LimboBalance: 5000,
					Commitments:  &lnrpc.PendingChannelsResponse_Commitments{},
					ClosingTxid:  "close-tx",
				},
			},
		},
		peers: &lnrpc.ListPeersResponse{},
	})

	got, err := exec.NodeHealth(context.Background())
	if err != nil {
		t.Fatalf("NodeHealth: %v", err)
	}
	if got.OverallStatus != "degraded" || got.CriticalCount != 0 ||
		got.WarningCount != 1 || got.AlertCount != 1 {

		t.Fatalf("unexpected waiting-close snapshot: %+v", got)
	}
	alert := got.Alerts[0]
	if alert.ID != "channel:waiting-close:waiting:1" ||
		alert.Severity != "warning" ||
		alert.Message != "Channel waiting to close" {

		t.Fatalf("unexpected waiting-close alert: %+v", alert)
	}
}

func TestLNDExecutorNodeHealthRequiresConfiguredNetwork(t *testing.T) {
	exec := newTestHealthExecutor(&mockLNDHealthClient{
		info: &lnrpc.GetInfoResponse{
			Chains: []*lnrpc.Chain{{
				Chain:   "bitcoin",
				Network: "mainnet",
			}},
		},
		pending: &lnrpc.PendingChannelsResponse{},
		peers:   &lnrpc.ListPeersResponse{},
	})

	_, err := exec.NodeHealth(context.Background())
	if err == nil || !strings.Contains(err.Error(), "regtest") {
		t.Fatalf("expected regtest network error, got %v", err)
	}
}

func TestLNDExecutorNodeHealthSurfacesReadErrors(t *testing.T) {
	exec := newTestHealthExecutor(&mockLNDHealthClient{
		info: &lnrpc.GetInfoResponse{
			Chains: []*lnrpc.Chain{{
				Chain:   "bitcoin",
				Network: "regtest",
			}},
		},
		pendingErr: errors.New("pending failed"),
		peers:      &lnrpc.ListPeersResponse{},
	})

	_, err := exec.NodeHealth(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pending failed") {
		t.Fatalf("expected pending read error, got %v", err)
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

func TestRebalanceFeeLimitMsat(t *testing.T) {
	tests := []struct {
		name      string
		amountSat int64
		feePPM    int64
		want      int64
	}{
		{name: "zero fee", amountSat: 1000, feePPM: 0, want: 0},
		{name: "sub sat round up", amountSat: 1, feePPM: 500, want: 1},
		{name: "exact one sat", amountSat: 2000, feePPM: 500, want: 1000},
		{name: "exact larger", amountSat: 1_000_000, feePPM: 500, want: 500_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rebalanceFeeLimitMsat(tc.amountSat, tc.feePPM)
			if err != nil {
				t.Fatalf("rebalanceFeeLimitMsat: %v", err)
			}
			if got != tc.want {
				t.Fatalf("fee limit msat = %d, want %d", got, tc.want)
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

func TestVerifyRouteFeeUsesMsatLimit(t *testing.T) {
	route := &lnrpc.Route{TotalFeesMsat: 2}
	if err := verifyRouteFee(route, 1); err == nil ||
		!strings.Contains(err.Error(), "msat") {

		t.Fatalf("expected msat fee-limit rejection, got %v", err)
	}
	if err := verifyRouteFee(route, 2); err != nil {
		t.Fatalf("verifyRouteFee should allow exact msat limit: %v", err)
	}
}
