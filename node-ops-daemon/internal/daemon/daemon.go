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
	if err := os.MkdirAll(filepath.Dir(sockPath), 0700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	// Remove a stale socket from a previous run.
	_ = os.Remove(sockPath)

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
		_ = d.ledger.Record(ledger.Entry{
			RequestID: reqID,
			Action:    req.Action,
			Params:    string(req.Params),
			Status:    "rejected",
			Reason:    "killswitch active",
			CreatedAt: time.Now(),
		})
		return Response{
			Status:    "error",
			RequestID: reqID,
			Reason:    "killswitch active: all execution is halted",
		}
	}

	switch req.Action {
	case "execute_fee_set":
		return d.handleFeeSet(reqID, req.Params)
	case "approve":
		return d.handleApprove(reqID, req.Params)
	case "reject":
		return d.handleReject(reqID, req.Params)
	case "list_pending":
		return d.handleListPending(reqID)
	case "status":
		return Response{
			Status:    "ok",
			RequestID: reqID,
			Result:    map[string]string{"state": "running"},
		}
	default:
		return Response{
			Status:    "error",
			RequestID: reqID,
			Reason:    fmt.Sprintf("unknown action: %q", req.Action),
		}
	}
}

// feeSetParams are the parameters for the execute_fee_set action.
type feeSetParams struct {
	ChanID   uint64 `json:"chan_id"`
	BaseMsat int64  `json:"base_msat"`
	FeePpm   int64  `json:"fee_ppm"`
	OldPpm   int64  `json:"old_ppm"` // current fee rate; used to compute delta
}

func (d *Daemon) handleFeeSet(reqID string, raw json.RawMessage) Response {
	var p feeSetParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return Response{Status: "error", RequestID: reqID, Reason: "invalid params: " + err.Error()}
	}

	delta := p.FeePpm - p.OldPpm
	if err := d.limits.CheckFeeDelta(delta); err != nil {
		_ = d.ledger.Record(ledger.Entry{
			RequestID: reqID, Action: "execute_fee_set",
			Params: string(raw), Status: "rejected", Reason: err.Error(), CreatedAt: time.Now(),
		})
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}

	absDelta := delta
	if absDelta < 0 {
		absDelta = -absDelta
	}
	needsApproval := d.cfg.Approval.RequireApproval ||
		absDelta > d.cfg.Approval.AutoExecuteBelowPpmDelta

	if !needsApproval {
		if err := d.executor.ExecuteFeeSet(context.Background(), executor.FeeSetRequest{
			ChanID: p.ChanID, BaseMsat: p.BaseMsat, FeePpm: p.FeePpm,
		}); err != nil {
			_ = d.ledger.Record(ledger.Entry{
				RequestID: reqID, Action: "execute_fee_set",
				Params: string(raw), Status: "failed", Reason: err.Error(), CreatedAt: time.Now(),
			})
			return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
		}
		_ = d.ledger.Record(ledger.Entry{
			RequestID: reqID, Action: "execute_fee_set",
			Params: string(raw), Status: "executed", CreatedAt: time.Now(),
		})
		return Response{Status: "ok", RequestID: reqID,
			Result: map[string]interface{}{"executed": true, "chan_id": p.ChanID}}
	}

	d.queue.Enqueue(queue.Item{
		RequestID: reqID, Action: "execute_fee_set",
		Params: string(raw), CreatedAt: time.Now(),
	})
	_ = d.ledger.Record(ledger.Entry{
		RequestID: reqID, Action: "execute_fee_set",
		Params: string(raw), Status: "pending", CreatedAt: time.Now(),
	})
	return Response{
		Status: "pending", RequestID: reqID,
		Result: map[string]interface{}{"queued": true, "request_id": reqID},
	}
}

type approveParams struct {
	RequestID string `json:"request_id"`
}

func (d *Daemon) handleApprove(reqID string, raw json.RawMessage) Response {
	var p approveParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return Response{Status: "error", RequestID: reqID, Reason: "invalid params: " + err.Error()}
	}
	item, ok := d.queue.Approve(p.RequestID)
	if !ok {
		return Response{Status: "error", RequestID: reqID,
			Reason: fmt.Sprintf("request_id %q not found or already decided", p.RequestID)}
	}
	_ = d.ledger.Record(ledger.Entry{
		RequestID: reqID, Action: "approve",
		Params:    fmt.Sprintf(`{"approved_id":%q}`, p.RequestID),
		Status:    "ok", CreatedAt: time.Now(),
	})
	return Response{Status: "ok", RequestID: reqID,
		Result: map[string]string{"approved": item.RequestID}}
}

type rejectParams struct {
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"`
}

func (d *Daemon) handleReject(reqID string, raw json.RawMessage) Response {
	var p rejectParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return Response{Status: "error", RequestID: reqID, Reason: "invalid params: " + err.Error()}
	}
	item, ok := d.queue.Reject(p.RequestID, p.Reason)
	if !ok {
		return Response{Status: "error", RequestID: reqID,
			Reason: fmt.Sprintf("request_id %q not found or already decided", p.RequestID)}
	}
	_ = d.ledger.Record(ledger.Entry{
		RequestID: reqID, Action: "reject",
		Params:    fmt.Sprintf(`{"rejected_id":%q}`, p.RequestID),
		Status:    "ok", Reason: p.Reason, CreatedAt: time.Now(),
	})
	return Response{Status: "ok", RequestID: reqID,
		Result: map[string]string{"rejected": item.RequestID}}
}

func (d *Daemon) handleListPending(reqID string) Response {
	items := d.queue.ListPending()
	if items == nil {
		items = []queue.Item{}
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
