#!/usr/bin/env python3
"""Observe node-ops daemon state without performing Lightning writes."""

from __future__ import annotations

import argparse

from node_ops_daemon import daemon_request, default_daemon_socket, print_json


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", default=default_daemon_socket())
    parser.add_argument("--timeout", type=float, default=2.0)
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("status", help="read daemon status and kill-switch state")
    sub.add_parser("pending", help="list pending approval requests")

    audit = sub.add_parser("audit", help="query append-only audit ledger")
    audit.add_argument("--request-id", default="")
    audit.add_argument("--action", default="")
    audit.add_argument("--status", default="")
    audit.add_argument("--limit", type=int, default=50)
    audit.add_argument("--offset", type=int, default=0)
    audit.add_argument("--oldest-first", action="store_true")

    args = parser.parse_args()
    if args.command == "status":
        resp = daemon_request(args.socket, "status", {}, timeout=args.timeout)
    elif args.command == "pending":
        resp = daemon_request(args.socket, "list_pending", {}, timeout=args.timeout)
    else:
        params = {
            "limit": args.limit,
            "offset": args.offset,
            "newest_first": not args.oldest_first,
        }
        if args.request_id:
            params["request_id"] = args.request_id
        if args.action:
            params["action"] = args.action
        if args.status:
            params["status"] = args.status
        resp = daemon_request(args.socket, "query_audit_log", params, timeout=args.timeout)

    print_json(resp)
    return 0 if resp.get("status") != "error" else 1


if __name__ == "__main__":
    raise SystemExit(main())
