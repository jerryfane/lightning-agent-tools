// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

package executor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

// LNDConfig holds the daemon-owned LND connection settings.
type LNDConfig struct {
	RPCAddress      string
	MacaroonPath    string
	TLSCertPath     string
	RequiredNetwork string
	DialTimeout     time.Duration
	RequestTimeout  time.Duration
}

// LNDExecutor applies daemon-approved operations through LND gRPC using only
// the scoped node-ops macaroon.
type LNDExecutor struct {
	conn            *grpc.ClientConn
	client          lnrpc.LightningClient
	requiredNetwork string
	requestTimeout  time.Duration
}

// NewLNDExecutor creates a concrete NodeExecutor backed by LND gRPC.
func NewLNDExecutor(ctx context.Context, cfg LNDConfig) (*LNDExecutor, error) {
	if strings.TrimSpace(cfg.RPCAddress) == "" {
		return nil, fmt.Errorf("lnd rpc address must not be empty")
	}
	if strings.TrimSpace(cfg.MacaroonPath) == "" {
		return nil, fmt.Errorf("macaroon path must not be empty")
	}
	if strings.TrimSpace(cfg.TLSCertPath) == "" {
		return nil, fmt.Errorf("tls cert path must not be empty")
	}
	if strings.TrimSpace(cfg.RequiredNetwork) == "" {
		return nil, fmt.Errorf("required network must not be empty")
	}
	if strings.TrimSpace(cfg.RequiredNetwork) != "regtest" {
		return nil, fmt.Errorf("required network must be regtest for gated writes")
	}
	if cfg.DialTimeout <= 0 {
		return nil, fmt.Errorf("dial timeout must be positive")
	}
	if cfg.RequestTimeout <= 0 {
		return nil, fmt.Errorf("request timeout must be positive")
	}

	tlsCreds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("load tls cert: %w", err)
	}
	macCreds, err := loadMacaroonCredential(cfg.MacaroonPath)
	if err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	conn, err := grpc.DialContext(
		dialCtx, cfg.RPCAddress,
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithPerRPCCredentials(macCreds),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("dial lnd: %w", err)
	}

	return &LNDExecutor{
		conn:            conn,
		client:          lnrpc.NewLightningClient(conn),
		requiredNetwork: strings.TrimSpace(cfg.RequiredNetwork),
		requestTimeout:  cfg.RequestTimeout,
	}, nil
}

func loadMacaroonCredential(path string) (macaroons.MacaroonCredential, error) {
	info, err := os.Stat(path)
	if err != nil {
		return macaroons.MacaroonCredential{},
			fmt.Errorf("stat macaroon: %w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return macaroons.MacaroonCredential{},
			fmt.Errorf("macaroon %s must not be group/other readable", path)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return macaroons.MacaroonCredential{},
			fmt.Errorf("read macaroon: %w", err)
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(body); err != nil {
		return macaroons.MacaroonCredential{},
			fmt.Errorf("decode macaroon: %w", err)
	}
	cred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return macaroons.MacaroonCredential{},
			fmt.Errorf("clone macaroon credential: %w", err)
	}
	return cred, nil
}

// Close releases the underlying gRPC connection.
func (e *LNDExecutor) Close() error {
	return e.conn.Close()
}

// CurrentFeePolicy returns the local channel policy owned by the node.
func (e *LNDExecutor) CurrentFeePolicy(ctx context.Context,
	chanID uint64) (FeePolicy, error) {

	ctx, cancel := e.withRequestTimeout(ctx)
	defer cancel()

	if err := e.requireNetwork(ctx); err != nil {
		return FeePolicy{}, err
	}
	policy, err := e.currentRoutingPolicy(ctx, chanID)
	if err != nil {
		return FeePolicy{}, err
	}
	return FeePolicy{
		BaseMsat:      policy.GetFeeBaseMsat(),
		FeePpm:        policy.GetFeeRateMilliMsat(),
		TimeLockDelta: policy.GetTimeLockDelta(),
	}, nil
}

// ExecuteFeeSet applies a bounded fee policy update to one channel.
func (e *LNDExecutor) ExecuteFeeSet(ctx context.Context, req FeeSetRequest) error {
	ctx, cancel := e.withRequestTimeout(ctx)
	defer cancel()

	if err := e.requireNetwork(ctx); err != nil {
		return err
	}
	if req.FeePpm < 0 || req.FeePpm > math.MaxUint32 {
		return fmt.Errorf("fee_ppm %d is outside uint32 range", req.FeePpm)
	}

	current, err := e.currentRoutingPolicy(ctx, req.ChanID)
	if err != nil {
		return err
	}
	chanPoint, err := e.channelPoint(ctx, req.ChanID)
	if err != nil {
		return err
	}

	resp, err := e.client.UpdateChannelPolicy(ctx, &lnrpc.PolicyUpdateRequest{
		Scope: &lnrpc.PolicyUpdateRequest_ChanPoint{
			ChanPoint: chanPoint,
		},
		BaseFeeMsat:   req.BaseMsat,
		FeeRatePpm:    uint32(req.FeePpm),
		TimeLockDelta: current.GetTimeLockDelta(),
	})
	if err != nil {
		return fmt.Errorf("update channel policy: %w", err)
	}
	if len(resp.GetFailedUpdates()) > 0 {
		return fmt.Errorf("update channel policy failed: %s",
			formatFailedUpdates(resp.GetFailedUpdates()))
	}
	return nil
}

func (e *LNDExecutor) NodeHealth(ctx context.Context) (NodeHealthSnapshot, error) {
	return NodeHealthSnapshot{}, ErrNotImplemented
}

func (e *LNDExecutor) withRequestTimeout(ctx context.Context) (context.Context,
	context.CancelFunc) {

	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, e.requestTimeout)
}

func (e *LNDExecutor) requireNetwork(ctx context.Context) error {
	info, err := e.client.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("get lnd info: %w", err)
	}
	for _, chain := range info.GetChains() {
		if chain.GetNetwork() == e.requiredNetwork {
			return nil
		}
	}
	return fmt.Errorf("lnd network is not %s", e.requiredNetwork)
}

func (e *LNDExecutor) currentRoutingPolicy(ctx context.Context,
	chanID uint64) (*lnrpc.RoutingPolicy, error) {

	info, err := e.client.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("get lnd info: %w", err)
	}
	edge, err := e.client.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
		ChanId: chanID,
	})
	if err != nil {
		return nil, fmt.Errorf("get channel info: %w", err)
	}

	switch info.GetIdentityPubkey() {
	case edge.GetNode1Pub():
		if edge.GetNode1Policy() == nil {
			return nil, fmt.Errorf("local node policy missing for channel %d",
				chanID)
		}
		return edge.GetNode1Policy(), nil
	case edge.GetNode2Pub():
		if edge.GetNode2Policy() == nil {
			return nil, fmt.Errorf("local node policy missing for channel %d",
				chanID)
		}
		return edge.GetNode2Policy(), nil
	default:
		return nil, fmt.Errorf("channel %d is not owned by local node %s",
			chanID, info.GetIdentityPubkey())
	}
}

func (e *LNDExecutor) channelPoint(ctx context.Context,
	chanID uint64) (*lnrpc.ChannelPoint, error) {

	resp, err := e.client.ListChannels(ctx, &lnrpc.ListChannelsRequest{})
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	for _, ch := range resp.GetChannels() {
		if ch.GetChanId() != chanID {
			continue
		}
		cp, err := parseChannelPoint(ch.GetChannelPoint())
		if err != nil {
			return nil, err
		}
		return cp, nil
	}
	return nil, fmt.Errorf("channel %d not found in local channels", chanID)
}

func parseChannelPoint(value string) (*lnrpc.ChannelPoint, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
		return nil, fmt.Errorf("invalid channel point %q", value)
	}
	index, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid channel point output index: %w", err)
	}
	return &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{
			FundingTxidStr: parts[0],
		},
		OutputIndex: uint32(index),
	}, nil
}

func formatFailedUpdates(updates []*lnrpc.FailedUpdate) string {
	parts := make([]string, 0, len(updates))
	for _, update := range updates {
		if update.GetUpdateError() != "" {
			parts = append(parts, update.GetUpdateError())
			continue
		}
		parts = append(parts, update.GetReason().String())
	}
	if len(parts) == 0 {
		return errors.New("unknown failure").Error()
	}
	return strings.Join(parts, "; ")
}
