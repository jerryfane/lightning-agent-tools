// Copyright (c) 2025 Lightning Labs
// Distributed under the MIT license. See LICENSE for details.

// Package daemon implements the node-ops-daemon Unix-socket server.
//
// Wire format: each message is a 4-byte big-endian length followed by a
// JSON body. Requests carry {action, params}; responses carry
// {status, request_id, result?, reason?}.
package daemon

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/executor"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/killswitch"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/ledger"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/limits"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/queue"
)

const maxMessageBytes = 1 << 20 // 1 MiB

// Request is the incoming message from a client.
type Request struct {
	Action string          `json:"action"`
	Params json.RawMessage `json:"params"`
}

// Response is the outgoing reply to a client.
type Response struct {
	Status    string      `json:"status"`
	RequestID string      `json:"request_id"`
	Result    interface{} `json:"result,omitempty"`
	Reason    string      `json:"reason,omitempty"`
}

// Daemon is the long-lived node operations daemon.
type Daemon struct {
	cfg      *config.Config
	limits   *limits.Engine
	ledger   *ledger.Ledger
	queue    *queue.Queue
	executor executor.NodeExecutor
	execMu   sync.Mutex
}

// New creates a Daemon, initialises the limits engine and opens the ledger.
func New(cfg *config.Config, exec executor.NodeExecutor) (*Daemon, error) {
	eng, err := limits.New(cfg.Limits)
	if err != nil {
		return nil, fmt.Errorf("limits engine: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Storage.LedgerPath), 0700); err != nil {
		return nil, fmt.Errorf("create ledger dir: %w", err)
	}
	l, err := ledger.Open(cfg.Storage.LedgerPath)
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	return &Daemon{
		cfg:      cfg,
		limits:   eng,
		ledger:   l,
		queue:    queue.New(),
		executor: exec,
	}, nil
}

// Close releases the ledger handle.
func (d *Daemon) Close() error {
	return d.ledger.Close()
}

// Run listens on sockPath (Unix domain socket, mode 0600) and handles clients
// until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context, sockPath string) error {
	if err := prepareSocketDir(filepath.Dir(sockPath)); err != nil {
		return err
	}
	if err := removeStaleSocket(sockPath); err != nil {
		return err
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0600); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("accept: %w", err)
			}
		}
		go d.handleConn(conn)
	}
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	for {
		req, err := readMessage(conn)
		if err != nil {
			if err != io.EOF {
				_ = writeMessage(conn, Response{
					Status: "error",
					Reason: fmt.Sprintf("read error: %v", err),
				})
			}
			return
		}
		resp := d.dispatch(req)
		if err := writeMessage(conn, resp); err != nil {
			return
		}
	}
}

// dispatch routes a request to the appropriate handler and enforces the
// kill-switch before any execution.
func (d *Daemon) dispatch(req Request) Response {
	reqID := uuid.New().String()

	if killswitch.Active(d.cfg.Storage.KillswitchFile) {
		if err := d.record(reqID, req.Action, req.Params, "rejected",
			"killswitch active"); err != nil {

			return ledgerFailure(reqID, err)
		}
		return Response{
			Status:    "error",
			RequestID: reqID,
			Reason:    "killswitch active: all execution is halted",
		}
	}

	switch req.Action {
	case "execute_fee_set":
		return d.handleFeeSet(reqID, req.Params)
	case "list_pending":
		return d.handleListPending(reqID)
	case "status":
		if err := d.record(reqID, req.Action, req.Params, "ok", ""); err != nil {
			return ledgerFailure(reqID, err)
		}
		return Response{
			Status:    "ok",
			RequestID: reqID,
			Result:    map[string]string{"state": "running"},
		}
	default:
		reason := fmt.Sprintf("unknown action: %q", req.Action)
		if err := d.record(reqID, req.Action, req.Params, "rejected", reason); err != nil {
			return ledgerFailure(reqID, err)
		}
		return Response{
			Status:    "error",
			RequestID: reqID,
			Reason:    reason,
		}
	}
}

func (d *Daemon) record(reqID, action string, params json.RawMessage,
	status, reason string) error {

	return d.ledger.Record(ledger.Entry{
		RequestID: reqID,
		Action:    action,
		Params:    string(params),
		Status:    status,
		Reason:    reason,
		CreatedAt: time.Now(),
	})
}

func ledgerFailure(reqID string, err error) Response {
	return Response{
		Status:    "error",
		RequestID: reqID,
		Reason:    "audit ledger unavailable: " + err.Error(),
	}
}

// feeSetParams are the parameters for the execute_fee_set action.
type feeSetParams struct {
	ChanID   uint64 `json:"chan_id"`
	BaseMsat int64  `json:"base_msat"`
	FeePpm   int64  `json:"fee_ppm"`
}

type rawFeeSetParams struct {
	ChanID   *uint64 `json:"chan_id"`
	BaseMsat *int64  `json:"base_msat"`
	FeePpm   *int64  `json:"fee_ppm"`
}

type feeSetPolicyDelta struct {
	ppmDelta    int64
	baseChanged bool
}

func (d *Daemon) handleFeeSet(reqID string, raw json.RawMessage) Response {
	p, err := parseFeeSetParams(raw)
	if err != nil {
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			"invalid params: "+err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: "invalid params: " + err.Error()}
	}

	delta, err := d.feeSetPolicyDelta(context.Background(), p)
	if err != nil {
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}
	if err := d.limits.CheckFeeDelta(delta.ppmDelta); err != nil {
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}

	if !d.needsApproval(delta) {
		return d.executeFeeSet(reqID, raw, p)
	}

	return d.queueFeeSet(reqID, raw)
}

func parseFeeSetParams(raw json.RawMessage) (feeSetParams, error) {
	var rawParams rawFeeSetParams
	if err := json.Unmarshal(raw, &rawParams); err != nil {
		return feeSetParams{}, err
	}
	if rawParams.ChanID == nil {
		return feeSetParams{}, fmt.Errorf("missing chan_id")
	}
	if rawParams.BaseMsat == nil {
		return feeSetParams{}, fmt.Errorf("missing base_msat")
	}
	if rawParams.FeePpm == nil {
		return feeSetParams{}, fmt.Errorf("missing fee_ppm")
	}
	if *rawParams.ChanID == 0 {
		return feeSetParams{}, fmt.Errorf("chan_id must be non-zero")
	}
	if *rawParams.BaseMsat < 0 {
		return feeSetParams{}, fmt.Errorf("base_msat must be non-negative")
	}
	if *rawParams.FeePpm < 0 {
		return feeSetParams{}, fmt.Errorf("fee_ppm must be non-negative")
	}
	return feeSetParams{
		ChanID:   *rawParams.ChanID,
		BaseMsat: *rawParams.BaseMsat,
		FeePpm:   *rawParams.FeePpm,
	}, nil
}

func prepareSocketDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat socket dir %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("socket dir %s must not be a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("socket dir %s is not a directory", dir)
	}
	if err := checkSocketDirOwner(dir, info); err != nil {
		return err
	}
	if info.Mode().Perm()&0077 == 0 {
		return nil
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("secure socket dir %s: %w", dir, err)
	}
	info, err = os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat socket dir %s: %w", dir, err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("socket dir %s has unsafe permissions %03o", dir, info.Mode().Perm())
	}
	return nil
}

func checkSocketDirOwner(dir string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect socket dir owner %s: unsupported stat type", dir)
	}
	uid := uint32(os.Geteuid())
	if stat.Uid != uid {
		return fmt.Errorf("socket dir %s owner uid %d does not match process uid %d",
			dir, stat.Uid, uid)
	}
	return nil
}

func removeStaleSocket(sockPath string) error {
	info, err := os.Lstat(sockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat socket %s: %w", sockPath, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s (mode %s)",
			sockPath, info.Mode())
	}
	conn, err := net.DialTimeout("unix", sockPath, time.Second)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon socket already active at %s", sockPath)
	}
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket %s: %w", sockPath, err)
	}
	return nil
}

func (d *Daemon) feeSetPolicyDelta(ctx context.Context,
	p feeSetParams) (feeSetPolicyDelta, error) {

	current, err := d.executor.CurrentFeePolicy(ctx, p.ChanID)
	if err != nil {
		return feeSetPolicyDelta{}, fmt.Errorf("current fee lookup failed: %w", err)
	}
	return feeSetPolicyDelta{
		ppmDelta:    p.FeePpm - current.FeePpm,
		baseChanged: p.BaseMsat != current.BaseMsat,
	}, nil
}

func (d *Daemon) needsApproval(delta feeSetPolicyDelta) bool {
	ppmDelta := delta.ppmDelta
	if ppmDelta < 0 {
		ppmDelta = -ppmDelta
	}
	return d.cfg.Approval.RequireApproval || delta.baseChanged ||
		ppmDelta > d.cfg.Approval.AutoExecuteBelowPpmDelta
}

func (d *Daemon) executeFeeSet(reqID string, raw json.RawMessage,
	p feeSetParams) Response {

	d.execMu.Lock()
	defer d.execMu.Unlock()

	delta, err := d.feeSetPolicyDelta(context.Background(), p)
	if err != nil {
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}
	if err := d.limits.CheckFeeDelta(delta.ppmDelta); err != nil {
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}
	if d.needsApproval(delta) {
		return d.queueFeeSet(reqID, raw)
	}
	if err := d.limits.CheckChannelCooldown(p.ChanID); err != nil {
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}
	if killswitch.Active(d.cfg.Storage.KillswitchFile) {
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			"killswitch active"); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{
			Status:    "error",
			RequestID: reqID,
			Reason:    "killswitch active: all execution is halted",
		}
	}
	if err := d.record(reqID, "execute_fee_set", raw, "accepted", ""); err != nil {
		return ledgerFailure(reqID, err)
	}
	if err := d.executor.ExecuteFeeSet(context.Background(), executor.FeeSetRequest{
		ChanID: p.ChanID, BaseMsat: p.BaseMsat, FeePpm: p.FeePpm,
	}); err != nil {
		if recErr := d.record(reqID, "execute_fee_set", raw, "failed",
			err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}
	d.limits.RecordChannelOperation(p.ChanID)
	if err := d.record(reqID, "execute_fee_set", raw, "executed", ""); err != nil {
		return ledgerFailure(reqID, err)
	}
	return Response{Status: "ok", RequestID: reqID,
		Result: map[string]interface{}{"executed": true, "chan_id": p.ChanID}}
}

func (d *Daemon) queueFeeSet(reqID string, raw json.RawMessage) Response {
	if err := d.record(reqID, "execute_fee_set", raw, "pending", ""); err != nil {
		return ledgerFailure(reqID, err)
	}
	d.queue.Enqueue(queue.Item{
		RequestID: reqID, Action: "execute_fee_set",
		Params: string(raw), CreatedAt: time.Now(),
	})
	return Response{
		Status: "pending", RequestID: reqID,
		Result: map[string]interface{}{"queued": true, "request_id": reqID},
	}
}

func (d *Daemon) handleListPending(reqID string) Response {
	items := d.queue.ListPending()
	if items == nil {
		items = []queue.Item{}
	}
	if err := d.record(reqID, "list_pending", nil, "ok", ""); err != nil {
		return ledgerFailure(reqID, err)
	}
	return Response{Status: "ok", RequestID: reqID, Result: items}
}

// readMessage reads one length-prefixed JSON message from r.
func readMessage(r io.Reader) (Request, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return Request{}, err
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	if length > maxMessageBytes {
		return Request{}, fmt.Errorf("message too large: %d bytes", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return Request{}, err
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return Request{}, fmt.Errorf("decode request: %w", err)
	}
	return req, nil
}

// writeMessage encodes resp as length-prefixed JSON and writes it to w.
func writeMessage(w io.Writer, resp Response) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
