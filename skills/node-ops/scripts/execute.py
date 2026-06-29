#!/usr/bin/env python3
"""Submit gated node-ops requests or operator decisions to the daemon."""

from __future__ import annotations

import argparse

from node_ops_daemon import (
    daemon_request,
    default_daemon_socket,
    default_operator_socket,
    default_operator_token_file,
    non_negative_int,
    positive_int,
    print_json,
    read_operator_token,
)


def add_fee_set(sub: argparse._SubParsersAction[argparse.ArgumentParser]) -> None:
    cmd = sub.add_parser("fee-set", help="submit execute_fee_set to the daemon")
    cmd.add_argument("--chan-id", type=positive_int, required=True)
    cmd.add_argument("--base-msat", type=non_negative_int, required=True)
    cmd.add_argument("--fee-ppm", type=non_negative_int, required=True)


def add_rebalance(sub: argparse._SubParsersAction[argparse.ArgumentParser]) -> None:
    cmd = sub.add_parser("rebalance", help="submit execute_rebalance to the daemon")
    cmd.add_argument("--outgoing-chan-id", type=positive_int, required=True)
    cmd.add_argument("--incoming-chan-id", type=positive_int, required=True)
    cmd.add_argument("--amount-sat", type=positive_int, required=True)
    cmd.add_argument("--max-fee-ppm", type=non_negative_int, required=True)


def add_decision(
    sub: argparse._SubParsersAction[argparse.ArgumentParser],
    name: str,
    help_text: str,
) -> None:
    cmd = sub.add_parser(name, help=help_text)
    cmd.add_argument("--request-id", required=True)
    if name.startswith("deny-"):
        cmd.add_argument("--reason", default="operator denied")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", default=default_daemon_socket())
    parser.add_argument("--operator-socket", default=default_operator_socket())
    parser.add_argument("--operator-token-file", default=default_operator_token_file())
    parser.add_argument("--timeout", type=float, default=2.0)
    sub = parser.add_subparsers(dest="command", required=True)

    add_fee_set(sub)
    add_rebalance(sub)
    add_decision(sub, "approve-fee-set", "approve a pending execute_fee_set request")
    add_decision(sub, "deny-fee-set", "deny a pending execute_fee_set request")
    add_decision(sub, "approve-rebalance", "approve a pending execute_rebalance request")
    add_decision(sub, "deny-rebalance", "deny a pending execute_rebalance request")
    args = parser.parse_args()

    if args.command == "fee-set":
        resp = daemon_request(
            args.socket,
            "execute_fee_set",
            {
                "chan_id": args.chan_id,
                "base_msat": args.base_msat,
                "fee_ppm": args.fee_ppm,
            },
            timeout=args.timeout,
        )
    elif args.command == "rebalance":
        resp = daemon_request(
            args.socket,
            "execute_rebalance",
            {
                "outgoing_chan_id": args.outgoing_chan_id,
                "incoming_chan_id": args.incoming_chan_id,
                "amount_sat": args.amount_sat,
                "max_fee_ppm": args.max_fee_ppm,
            },
            timeout=args.timeout,
        )
    else:
        operator_action = {
            "approve-fee-set": "approve_fee_set",
            "deny-fee-set": "deny_fee_set",
            "approve-rebalance": "approve_rebalance",
            "deny-rebalance": "deny_rebalance",
        }[args.command]
        params = {"request_id": args.request_id}
        if args.command.startswith("deny-"):
            params["reason"] = args.reason
        resp = daemon_request(
            args.operator_socket,
            operator_action,
            params,
            operator_token=read_operator_token(args.operator_token_file),
            timeout=args.timeout,
        )

    print_json(resp)
    return 0 if resp.get("status") != "error" else 1


if __name__ == "__main__":
    raise SystemExit(main())
