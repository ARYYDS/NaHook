#!/usr/bin/env python3
"""Join client.log and backend.log from T0 and print one row per run.

delta_s = time of the backend's terminal event minus the time curl gave up.
Propagated means: hold -> peer_closed with delta_s <= 1.0;
                  stream -> write_failed with delta_s <= 2.0.
"""
import json
import re

DIR = "/root/nahook-t0/"
client = {}
for line in open(DIR + "client.log", encoding="utf-8"):
    row = json.loads(line)
    if "path" in row:
        client[(row["path"], row["i"])] = row

requests, terminal = {}, {}
for line in open(DIR + "backend.log", encoding="utf-8"):
    row = json.loads(line)
    if row.get("event") == "request":
        match = re.match(r"\S+ /([^/]+)/t(\d+)", row.get("line") or "")
        if match:
            requests[row["peer"]] = (match.group(1), int(match.group(2)))
    elif row.get("kind") in ("hold", "stream"):
        terminal[row["peer"]] = row

print(f"{'path':16}{'i':>2}{'rc':>4}  {'backend_event':22}{'delta_s':>8}  propagated")
for peer, key in sorted(requests.items(), key=lambda kv: kv[1]):
    c, t = client.get(key), terminal.get(peer)
    if not c or not t:
        print(f"{key[0]:16}{key[1]:>2}  missing data")
        continue
    delta = t["t"] - c["curl_end"]
    limit = 1.0 if key[0].startswith("hold") else 2.0
    wanted = "peer_closed" if key[0].startswith("hold") else "write_failed"
    ok = t["event"] == wanted and delta <= limit
    print(f"{key[0]:16}{key[1]:>2}{c['rc']:>4}  {t['event']:22}{delta:>+8.2f}  {'yes' if ok else 'no'}")
