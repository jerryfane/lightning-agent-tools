#!/usr/bin/env python3
"""Render reviewable node-ops payloads without submitting writes."""

from __future__ import annotations

import argparse

from node_ops_daemon import (
    daemon_request,
    default_daemon_socket,
    non_negative_int,
    positive_int,
    print_json,
)


def add_fee_set(sub: argparse._SubParsersAction[argparse.ArgumentParser]) -> None:
    cmd = sub.add_parser("fee-set", help="render a gated fee-set payload")
    cmd.add_argument("--chan-id", type=positive_int, required=True)
    cmd.add_argument("--base-msat", type=non_negative_int, required=True)
    cmd.add_argument("--fee-ppm", type=non_negative_int, required=True)


def add_rebalance(sub: argparse._SubParsersAction[argparse.ArgumentParser]) -> None:
    cmd = sub.add_parser("rebalance", help="render a gated rebalance payload")
    cmd.add_argument("--outgoing-chan-id", type=positive_int, required=True)
    cmd.add_argument("--incoming-chan-id", type=positive_int, required=True)
    cmd.add_argument("--amount-sat", type=positive_int, required=True)
    cmd.add_argument("--max-fee-ppm", type=non_negative_int, required=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", default=default_daemon_socket())
    parser.add_argument("--timeout", type=float, default=2.0)
    sub = parser.add_subparsers(dest="command", required=True)
    add_fee_set(sub)
    add_rebalance(sub)
    args = parser.parse_args()

    status = daemon_request(args.socket, "status", {}, timeout=args.timeout)
    pending = daemon_request(args.socket, "list_pending", {}, timeout=args.timeout)
    if status.get("status") == "error":
        print_json(status)
        return 1
    if pending.get("status") == "error":
        print_json(pending)
        return 1

    if args.command == "fee-set":
        daemon_action = "execute_fee_set"
        mcp_tool = "lnc_execute_fee_set"
        payload = {
            "chan_id": args.chan_id,
            "base_msat": args.base_msat,
            "fee_ppm": args.fee_ppm,
        }
        checklist = [
            "Compare proposed fee_ppm with current policy from lnc_propose_fees.",
            "Confirm ppm delta, daily fee-ppm budget, per-channel cooldown, and kill-switch.",
            "Submit only through lnc_execute_fee_set or scripts/execute.py fee-set.",
        ]
    else:
        daemon_action = "execute_rebalance"
        mcp_tool = "lnc_execute_rebalance"
        payload = {
            "outgoing_chan_id": args.outgoing_chan_id,
            "incoming_chan_id": args.incoming_chan_id,
            "amount_sat": args.amount_sat,
            "max_fee_ppm": args.max_fee_ppm,
        }
        checklist = [
            "Outgoing channel should have excess local balance; incoming should need local balance.",
            "Confirm daily sat budget, max_fee_ppm, both channel cooldowns, and kill-switch.",
            "Submit only through lnc_execute_rebalance or scripts/execute.py rebalance.",
        ]

    print_json(
        {
            "daemon": {
                "status": status.get("result", status),
                "pending": pending.get("result", []),
            },
            "proposal": {
                "mcp_tool": mcp_tool,
                "daemon_action": daemon_action,
                "params": payload,
                "expected_submit_status": "pending",
                "review_checklist": checklist,
            },
        }
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
