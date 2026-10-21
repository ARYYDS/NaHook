#!/usr/bin/env python3
"""T0 backend for the Caddy 2.6.2 client-disconnect experiment.

19082 (hold):   reads the request, never answers, and records when Caddy
                closes the upstream connection (or that it stayed open).
19083 (stream): answers 200 text/event-stream with one event per second and
                records when a write fails (Caddy closed the upstream side).

Every observation is one JSON line in /root/nahook-t0/backend.log.
Only listens on 127.0.0.1. Standard library only.
"""
import json
import socket
import threading
import time

LOG = "/root/nahook-t0/backend.log"
HOLD_SECONDS = 20
STREAM_EVENTS = 20
_lock = threading.Lock()


def log(**fields):
    fields["t"] = round(time.time(), 3)
    with _lock, open(LOG, "a", encoding="utf-8") as f:
        f.write(json.dumps(fields) + "\n")


def read_request(conn):
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = conn.recv(4096)
        if not chunk:
            return None
        data += chunk
    head, _, body = data.partition(b"\r\n\r\n")
    lines = head.split(b"\r\n")
    length = 0
    for line in lines[1:]:
        key, _, value = line.partition(b":")
        if key.strip().lower() == b"content-length":
            length = int(value.strip())
    while len(body) < length:
        chunk = conn.recv(4096)
        if not chunk:
            break
        body += chunk
    return lines[0].decode("latin-1")


def hold(conn, peer):
    line = read_request(conn)
    log(kind="hold", event="request", line=line, peer=peer)
    conn.settimeout(HOLD_SECONDS)
    try:
        got = conn.recv(1)
        log(kind="hold", event="peer_closed" if got == b"" else "peer_sent", peer=peer)
    except socket.timeout:
        log(kind="hold", event="timeout_still_open", peer=peer)
    except OSError as err:
        log(kind="hold", event="peer_reset", err=str(err), peer=peer)
    finally:
        conn.close()


def stream(conn, peer):
    line = read_request(conn)
    log(kind="stream", event="request", line=line, peer=peer)
    sent = 0
    try:
        conn.sendall(
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: text/event-stream\r\n"
            b"Cache-Control: no-cache\r\n"
            b"Connection: close\r\n\r\n"
        )
        for i in range(STREAM_EVENTS):
            conn.sendall(f"data: {i}\n\n".encode())
            sent = i + 1
            time.sleep(1)
        log(kind="stream", event="completed", sent=sent, peer=peer)
    except OSError as err:
        log(kind="stream", event="write_failed", sent=sent, err=str(err), peer=peer)
    finally:
        conn.close()


def serve(port, handler):
    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("127.0.0.1", port))
    sock.listen(16)
    log(kind="backend", event="listening", port=port)
    while True:
        conn, addr = sock.accept()
        threading.Thread(target=handler, args=(conn, addr[1]), daemon=True).start()


if __name__ == "__main__":
    threading.Thread(target=serve, args=(19082, hold), daemon=True).start()
    serve(19083, stream)
