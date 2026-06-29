#!/usr/bin/env python3
"""Small client for the node-ops-daemon length-prefixed JSON socket."""

from __future__ import annotations

import json
import os
import socket
import stat
import struct
import sys
from pathlib import Path
from typing import Any


DEFAULT_DAEMON_SOCKET = "~/.node-ops/daemon.sock"
DEFAULT_OPERATOR_SOCKET = "~/.node-ops/operator.sock"
DEFAULT_OPERATOR_TOKEN_FILE = "~/.node-ops/operator.token"


def expand_path(value: str) -> str:
    return str(Path(value).expanduser())


def default_daemon_socket() -> str:
    return expand_path(os.environ.get("NODE_OPS_DAEMON_SOCKET", DEFAULT_DAEMON_SOCKET))


def default_operator_socket() -> str:
    return expand_path(os.environ.get("NODE_OPS_OPERATOR_SOCKET", DEFAULT_OPERATOR_SOCKET))


def default_operator_token_file() -> str:
    return expand_path(
        os.environ.get("NODE_OPS_OPERATOR_TOKEN_FILE", DEFAULT_OPERATOR_TOKEN_FILE)
    )


def read_operator_token(path: str) -> str:
    token_path = Path(path).expanduser()
    info = token_path.stat()
    if stat.S_IMODE(info.st_mode) & 0o077:
        raise RuntimeError(f"operator token file {token_path} must not be group/other readable")
    token = token_path.read_text(encoding="utf-8").strip()
    if not token:
        raise RuntimeError(f"operator token file {token_path} is empty")
    return token


def daemon_request(
    socket_path: str,
    action: str,
    params: dict[str, Any] | None = None,
    *,
    operator_token: str | None = None,
    timeout: float = 2.0,
) -> dict[str, Any]:
    body: dict[str, Any] = {"action": action, "params": params or {}}
    if operator_token is not None:
        body["operator_token"] = operator_token
    encoded = json.dumps(body, separators=(",", ":")).encode("utf-8")

    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(timeout)
        conn.connect(expand_path(socket_path))
        conn.sendall(struct.pack(">I", len(encoded)) + encoded)
        size = _read_exact(conn, 4)
        response_len = struct.unpack(">I", size)[0]
        response = _read_exact(conn, response_len)

    return json.loads(response.decode("utf-8"))


def _read_exact(conn: socket.socket, size: int) -> bytes:
    chunks: list[bytes] = []
    remaining = size
    while remaining:
        chunk = conn.recv(remaining)
        if not chunk:
            raise EOFError("daemon closed socket before full response")
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def print_json(value: Any) -> None:
    json.dump(value, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")


def positive_int(value: str) -> int:
    parsed = int(value, 10)
    if parsed <= 0:
        raise ValueError("must be positive")
    return parsed


def non_negative_int(value: str) -> int:
    parsed = int(value, 10)
    if parsed < 0:
        raise ValueError("must be non-negative")
    return parsed
