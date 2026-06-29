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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/config"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/executor"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/killswitch"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/ledger"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/limits"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/monitor"
	"github.com/lightninglabs/lightning-agent-kit/node-ops-daemon/internal/queue"
)

const maxMessageBytes = 1 << 20 // 1 MiB

const (
	defaultAuditQueryLimit = 50
	maxAuditQueryLimit     = 500
	maxAuditParamBytes     = 4096
)

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
	monitor  *monitor.Monitor
	execMu   sync.Mutex
}

// New creates a Daemon, initialises the limits engine and opens the ledger.
func New(cfg *config.Config, exec executor.NodeExecutor) (*Daemon, error) {
	if err := prepareLedgerDir(filepath.Dir(cfg.Storage.LedgerPath)); err != nil {
		return nil, err
	}
	if err := prepareLimitsStateDir(filepath.Dir(cfg.Storage.LimitsStatePath)); err != nil {
		return nil, err
	}
	eng, err := limits.NewPersistent(cfg.Limits, cfg.Storage.LimitsStatePath)
	if err != nil {
		return nil, fmt.Errorf("limits engine: %w", err)
	}
	l, err := ledger.Open(cfg.Storage.LedgerPath)
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	mon, err := newHealthMonitor(cfg, exec)
	if err != nil {
		l.Close()
		return nil, err
	}
	return &Daemon{
		cfg:      cfg,
		limits:   eng,
		ledger:   l,
		queue:    queue.New(),
		executor: exec,
		monitor:  mon,
	}, nil
}

func newHealthMonitor(cfg *config.Config,
	exec executor.NodeExecutor) (*monitor.Monitor, error) {

	if !cfg.Monitor.Enabled {
		return nil, nil
	}
	if _, ok := exec.(*executor.StubExecutor); ok {
		return nil, fmt.Errorf("monitor requires a concrete node_health reader")
	}
	pollInterval, err := time.ParseDuration(cfg.Monitor.PollInterval)
	if err != nil {
		return nil, fmt.Errorf("monitor poll interval: %w", err)
	}
	alertCooldown, err := time.ParseDuration(cfg.Monitor.AlertCooldown)
	if err != nil {
		return nil, fmt.Errorf("monitor alert cooldown: %w", err)
	}

	var publisher monitor.Publisher
	switch cfg.Monitor.AlertChannel {
	case "file":
		publisher, err = monitor.NewJSONLPublisher(cfg.Monitor.AlertPath)
	case "stdout":
		publisher, err = monitor.NewWriterPublisher(os.Stdout)
	default:
		err = fmt.Errorf("unsupported monitor alert channel %q",
			cfg.Monitor.AlertChannel)
	}
	if err != nil {
		return nil, fmt.Errorf("monitor alert publisher: %w", err)
	}

	mon, err := monitor.New(exec, publisher, monitor.Config{
		PollInterval:  pollInterval,
		AlertCooldown: alertCooldown,
	})
	if err != nil {
		return nil, fmt.Errorf("monitor: %w", err)
	}
	return mon, nil
}

// Close releases the ledger handle.
func (d *Daemon) Close() error {
	ledgerErr := d.ledger.Close()
	if closer, ok := d.executor.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil && ledgerErr == nil {
			return err
		}
	}
	return ledgerErr
}

// Run listens on sockPath (Unix domain socket, mode 0600) and handles clients
// until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context, sockPath string) error {
	return d.run(ctx, []socketSpec{{
		path:   sockPath,
		handle: d.handleConn,
	}})
}

// RunWithOperator listens on the model-callable execution socket plus a
// separate operator-only socket for approvals.
func (d *Daemon) RunWithOperator(ctx context.Context, sockPath,
	operatorSockPath string) error {

	specs := []socketSpec{{
		path:   sockPath,
		handle: d.handleConn,
	}}
	operatorSockPath = strings.TrimSpace(operatorSockPath)
	if operatorSockPath != "" {
		if operatorSockPath == sockPath {
			return fmt.Errorf("operator socket must differ from execution socket")
		}
		specs = append(specs, socketSpec{
			path:   operatorSockPath,
			handle: d.handleOperatorConn,
		})
	}
	return d.run(ctx, specs)
}

type socketSpec struct {
	path   string
	handle func(net.Conn)
}

func (d *Daemon) run(ctx context.Context, specs []socketSpec) error {
	listeners := make([]net.Listener, 0, len(specs))
	for _, spec := range specs {
		if err := prepareSocketDir(filepath.Dir(spec.path)); err != nil {
			return err
		}
		if err := removeStaleSocket(spec.path); err != nil {
			return err
		}

		ln, err := net.Listen("unix", spec.path)
		if err != nil {
			return fmt.Errorf("listen %s: %w", spec.path, err)
		}
		if err := os.Chmod(spec.path, 0600); err != nil {
			ln.Close()
			return fmt.Errorf("chmod socket: %w", err)
		}
		listeners = append(listeners, ln)
	}
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var monitorDone chan struct{}
	if d.monitor != nil {
		monitorDone = make(chan struct{})
		go func() {
			defer close(monitorDone)
			d.monitor.Run(runCtx)
		}()
		defer func() {
			cancel()
			<-monitorDone
		}()
	}

	go func() {
		<-runCtx.Done()
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()

	errCh := make(chan error, len(listeners))
	for i, ln := range listeners {
		spec := specs[i]
		go serveSocket(runCtx, ln, spec.path, spec.handle, errCh)
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		cancel()
		return err
	}
}

func serveSocket(ctx context.Context, ln net.Listener, path string,
	handle func(net.Conn), errCh chan<- error) {

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				errCh <- fmt.Errorf("accept %s: %w", path, err)
				return
			}
		}
		go handle(conn)
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

func (d *Daemon) handleOperatorConn(conn net.Conn) {
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
		resp := d.dispatchOperator(req)
		if err := writeMessage(conn, resp); err != nil {
			return
		}
	}
}

// dispatch routes a request to the appropriate handler. Write handlers enforce
// the kill-switch before execution; read-only status/inspection remains visible.
func (d *Daemon) dispatch(req Request) Response {
	reqID := uuid.New().String()

	switch req.Action {
	case "execute_fee_set":
		return d.handleFeeSet(reqID, req.Params)
	case "query_audit_log":
		return d.handleQueryAuditLog(reqID, req.Params)
	case "list_pending":
		return d.handleListPending(reqID)
	case "status":
		if err := d.record(reqID, req.Action, req.Params, "ok", ""); err != nil {
			return ledgerFailure(reqID, err)
		}
		return Response{
			Status:    "ok",
			RequestID: reqID,
			Result:    d.statusResult(),
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

// dispatchOperator routes requests from the separate human/operator boundary.
func (d *Daemon) dispatchOperator(req Request) Response {
	reqID := uuid.New().String()

	switch req.Action {
	case "approve_fee_set":
		return d.handleApproveFeeSet(reqID, req.Params)
	case "deny_fee_set":
		return d.handleDenyFeeSet(reqID, req.Params)
	case "list_pending":
		return d.handleListPending(reqID)
	case "status":
		if err := d.record(reqID, req.Action, req.Params, "ok", ""); err != nil {
			return ledgerFailure(reqID, err)
		}
		return Response{
			Status:    "ok",
			RequestID: reqID,
			Result:    d.statusResult(),
		}
	default:
		reason := fmt.Sprintf("unknown operator action: %q", req.Action)
		if err := d.record(reqID, req.Action, req.Params, "rejected", reason); err != nil {
			return ledgerFailure(reqID, err)
		}
		return Response{Status: "error", RequestID: reqID, Reason: reason}
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

type auditQueryParams struct {
	RequestID   string `json:"request_id"`
	Action      string `json:"action"`
	Status      string `json:"status"`
	Limit       *int   `json:"limit"`
	Offset      *int   `json:"offset"`
	NewestFirst *bool  `json:"newest_first"`
}

type auditQueryResult struct {
	Count   int                `json:"count"`
	Limit   int                `json:"limit"`
	Offset  int                `json:"offset"`
	Entries []auditEntryResult `json:"entries"`
}

type auditEntryResult struct {
	ID              int64           `json:"id"`
	RequestID       string          `json:"request_id"`
	Action          string          `json:"action"`
	Params          json.RawMessage `json:"params,omitempty"`
	ParamsPreview   string          `json:"params_preview,omitempty"`
	ParamsTruncated bool            `json:"params_truncated,omitempty"`
	Status          string          `json:"status"`
	Reason          string          `json:"reason"`
	CreatedAt       string          `json:"created_at"`
}

func (d *Daemon) handleQueryAuditLog(reqID string, raw json.RawMessage) Response {
	params, err := parseAuditQueryParams(raw)
	if err != nil {
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}

	entries, err := d.ledger.Query(ledger.QueryOptions{
		RequestID:   params.RequestID,
		Action:      params.Action,
		Status:      params.Status,
		Limit:       *params.Limit,
		Offset:      *params.Offset,
		NewestFirst: *params.NewestFirst,
	})
	if err != nil {
		return ledgerFailure(reqID, err)
	}

	result := auditQueryResult{
		Count:   len(entries),
		Limit:   *params.Limit,
		Offset:  *params.Offset,
		Entries: make([]auditEntryResult, len(entries)),
	}
	for i, entry := range entries {
		result.Entries[i] = auditEntryFromLedger(entry)
	}
	return Response{Status: "ok", RequestID: reqID, Result: result}
}

func parseAuditQueryParams(raw json.RawMessage) (auditQueryParams, error) {
	limit := defaultAuditQueryLimit
	offset := 0
	newestFirst := true
	params := auditQueryParams{
		Limit:       &limit,
		Offset:      &offset,
		NewestFirst: &newestFirst,
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return auditQueryParams{}, fmt.Errorf("invalid audit query params: %w", err)
		}
	}
	if params.Limit == nil {
		params.Limit = &limit
	}
	if *params.Limit < 1 || *params.Limit > maxAuditQueryLimit {
		return auditQueryParams{}, fmt.Errorf("limit must be between 1 and %d", maxAuditQueryLimit)
	}
	if params.Offset == nil {
		params.Offset = &offset
	}
	if *params.Offset < 0 {
		return auditQueryParams{}, fmt.Errorf("offset must be non-negative")
	}
	if params.NewestFirst == nil {
		params.NewestFirst = &newestFirst
	}
	params.RequestID = strings.TrimSpace(params.RequestID)
	params.Action = strings.TrimSpace(params.Action)
	params.Status = strings.TrimSpace(params.Status)
	return params, nil
}

func auditEntryFromLedger(entry ledger.Entry) auditEntryResult {
	params := strings.TrimSpace(entry.Params)
	if params == "" {
		params = "{}"
	}

	result := auditEntryResult{
		ID:        entry.ID,
		RequestID: entry.RequestID,
		Action:    entry.Action,
		Status:    entry.Status,
		Reason:    entry.Reason,
		CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339),
	}
	if len(params) > maxAuditParamBytes {
		result.ParamsPreview = params[:maxAuditParamBytes]
		result.ParamsTruncated = true
		return result
	}
	if json.Valid([]byte(params)) {
		result.Params = json.RawMessage(params)
		return result
	}
	result.ParamsPreview = params
	result.ParamsTruncated = true
	return result
}

func (d *Daemon) statusResult() map[string]string {
	monitorState := "disabled"
	if d.monitor != nil {
		monitorState = "enabled"
	}
	result := map[string]string{
		"state":      "running",
		"killswitch": "inactive",
		"monitor":    monitorState,
	}
	if killswitch.Active(d.cfg.Storage.KillswitchFile) {
		result["state"] = "stopped"
		result["killswitch"] = "active"
	}
	if d.monitor != nil {
		if msg, at, ok := d.monitor.LastError(); ok {
			result["monitor_error"] = msg
			result["monitor_error_at"] = at.Format(time.RFC3339)
		}
	}
	return result
}

func (d *Daemon) rejectIfKillSwitchActive(reqID, action string,
	raw json.RawMessage) (Response, bool) {

	if !killswitch.Active(d.cfg.Storage.KillswitchFile) {
		return Response{}, false
	}
	if recErr := d.record(reqID, action, raw, "rejected",
		"killswitch active"); recErr != nil {

		return ledgerFailure(reqID, recErr), true
	}
	return Response{
		Status:    "error",
		RequestID: reqID,
		Reason:    "killswitch active: all execution is halted",
	}, true
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

type approvalParams struct {
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"`
}

type feeSetPolicyDelta struct {
	ppmDelta    int64
	baseChanged bool
}

func (d *Daemon) handleFeeSet(reqID string, raw json.RawMessage) Response {
	if resp, stopped := d.rejectIfKillSwitchActive(reqID, "execute_fee_set",
		raw); stopped {

		return resp
	}

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
	if err := d.limits.CheckChannelCooldown(p.ChanID); err != nil {
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}

	if !d.needsApproval(delta) {
		return d.executeFeeSet(reqID, raw, p, false)
	}

	if resp, stopped := d.rejectIfKillSwitchActive(reqID, "execute_fee_set",
		raw); stopped {

		return resp
	}
	return d.queueFeeSet(reqID, raw)
}

func parseApprovalParams(raw json.RawMessage) (approvalParams, error) {
	var params approvalParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return approvalParams{}, err
	}
	params.RequestID = strings.TrimSpace(params.RequestID)
	params.Reason = strings.TrimSpace(params.Reason)
	if params.RequestID == "" {
		return approvalParams{}, fmt.Errorf("missing request_id")
	}
	return params, nil
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

func prepareLedgerDir(dir string) error {
	return preparePrivateDir("ledger", dir, false)
}

func prepareLimitsStateDir(dir string) error {
	return preparePrivateDir("limits state", dir, false)
}

func prepareSocketDir(dir string) error {
	return preparePrivateDir("socket", dir, true)
}

func preparePrivateDir(kind, dir string, fixExistingPerms bool) error {
	existed := true
	if _, err := os.Lstat(dir); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s dir %s: %w", kind, dir, err)
		}
		existed = false
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create %s dir: %w", kind, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat %s dir %s: %w", kind, dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s dir %s must not be a symlink", kind, dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s dir %s is not a directory", kind, dir)
	}
	if err := checkPrivateDirOwner(kind, dir, info); err != nil {
		return err
	}
	if info.Mode().Perm()&0077 == 0 {
		return nil
	}
	if existed && !fixExistingPerms {
		return fmt.Errorf("%s dir %s has unsafe permissions %03o", kind, dir, info.Mode().Perm())
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("secure %s dir %s: %w", kind, dir, err)
	}
	info, err = os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s dir %s: %w", kind, dir, err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%s dir %s has unsafe permissions %03o", kind, dir, info.Mode().Perm())
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

func (d *Daemon) handleApproveFeeSet(reqID string, raw json.RawMessage) Response {
	params, err := parseApprovalParams(raw)
	if err != nil {
		if recErr := d.record(reqID, "approve_fee_set", raw, "rejected",
			"invalid params: "+err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID,
			Reason: "invalid params: " + err.Error()}
	}

	item, ok := d.queue.Approve(params.RequestID)
	if !ok {
		reason := "pending request not found"
		if recErr := d.record(reqID, "approve_fee_set", raw, "rejected",
			reason); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: reason}
	}
	if recErr := d.record(reqID, "approve_fee_set", raw, "approved",
		item.RequestID); recErr != nil {

		return ledgerFailure(reqID, recErr)
	}

	p, err := parseFeeSetParams(json.RawMessage(item.Params))
	if err != nil {
		reason := "queued params invalid: " + err.Error()
		if recErr := d.record(item.RequestID, "execute_fee_set",
			json.RawMessage(item.Params), "failed", reason); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: reason}
	}

	execResp := d.executeFeeSet(
		item.RequestID, json.RawMessage(item.Params), p, true,
	)
	if execResp.Status != "ok" {
		return execResp
	}
	return Response{
		Status:    "ok",
		RequestID: reqID,
		Result: map[string]interface{}{
			"approved_request_id": item.RequestID,
			"execution":           execResp,
		},
	}
}

func (d *Daemon) handleDenyFeeSet(reqID string, raw json.RawMessage) Response {
	params, err := parseApprovalParams(raw)
	if err != nil {
		if recErr := d.record(reqID, "deny_fee_set", raw, "rejected",
			"invalid params: "+err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID,
			Reason: "invalid params: " + err.Error()}
	}

	reason := params.Reason
	if reason == "" {
		reason = "operator denied"
	}
	item, ok := d.queue.Reject(params.RequestID, reason)
	if !ok {
		if recErr := d.record(reqID, "deny_fee_set", raw, "rejected",
			"pending request not found"); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID,
			Reason: "pending request not found"}
	}
	if recErr := d.record(reqID, "deny_fee_set", raw, "denied",
		reason); recErr != nil {

		return ledgerFailure(reqID, recErr)
	}
	if recErr := d.record(item.RequestID, "execute_fee_set",
		json.RawMessage(item.Params), "denied", reason); recErr != nil {

		return ledgerFailure(reqID, recErr)
	}
	return Response{
		Status:    "ok",
		RequestID: reqID,
		Result: map[string]interface{}{
			"denied_request_id": item.RequestID,
			"reason":            reason,
		},
	}
}

func (d *Daemon) executeFeeSet(reqID string, raw json.RawMessage,
	p feeSetParams, approvalSatisfied bool) Response {

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
	if d.needsApproval(delta) && !approvalSatisfied {
		if resp, stopped := d.rejectIfKillSwitchActive(reqID, "execute_fee_set",
			raw); stopped {

			return resp
		}
		return d.queueFeeSet(reqID, raw)
	}
	if resp, stopped := d.rejectIfKillSwitchActive(reqID, "execute_fee_set",
		raw); stopped {

		return resp
	}
	reservation, err := d.limits.ReserveChannelOperation(p.ChanID)
	if err != nil {
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			err.Error()); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: err.Error()}
	}
	if killswitch.Active(d.cfg.Storage.KillswitchFile) {
		reason := "killswitch active: all execution is halted"
		if rollbackErr := d.limits.RollbackChannelOperation(reservation); rollbackErr != nil {
			reason = reason + "; limits rollback failed: " + rollbackErr.Error()
		}
		if recErr := d.record(reqID, "execute_fee_set", raw, "rejected",
			"killswitch active"); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: reason}
	}
	if err := d.record(reqID, "execute_fee_set", raw, "accepted", ""); err != nil {
		_ = d.limits.RollbackChannelOperation(reservation)
		return ledgerFailure(reqID, err)
	}
	if err := d.executor.ExecuteFeeSet(context.Background(), executor.FeeSetRequest{
		ChanID: p.ChanID, BaseMsat: p.BaseMsat, FeePpm: p.FeePpm,
	}); err != nil {
		reason := err.Error()
		if rollbackErr := d.limits.RollbackChannelOperation(reservation); rollbackErr != nil {
			reason = reason + "; limits rollback failed: " + rollbackErr.Error()
		}
		if recErr := d.record(reqID, "execute_fee_set", raw, "failed",
			reason); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "error", RequestID: reqID, Reason: reason}
	}
	if err := d.limits.RecordChannelOperation(p.ChanID); err != nil {
		reason := "limits state refresh failed: " + err.Error()
		if recErr := d.record(reqID, "execute_fee_set", raw, "executed",
			reason); recErr != nil {

			return ledgerFailure(reqID, recErr)
		}
		return Response{Status: "ok", RequestID: reqID,
			Result: map[string]interface{}{
				"executed": true,
				"chan_id":  p.ChanID,
				"warning":  reason,
			}}
	}
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
