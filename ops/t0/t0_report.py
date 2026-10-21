#!/usr/bin/env python3
"""Join client.log and backend.log from T0 and print one row per run.

delta_s = time of the backend's terminal event minus the time curl gave up.

result for the hold rows (Caddy is still waiting for the response header):
  yes   peer_closed or peer_reset with delta_s <= 1.0
  slow  peer_closed or peer_reset later than that
  no    timeout_still_open: the upstream connection stayed open for 20s

result for the stream rows (the response is already streaming):
  yes   write_failed with delta_s <= 3.0. The backend writes once a second and
        the first write after Caddy closed the connection can still succeed,
        so the failure shows up on the second write.
  slow  write_failed later than that
  no    completed: all 20 events were written although the client was gone
"""
import json
import re
from collections import Counter

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


def result(path, event, delta):
    if path.startswith("hold"):
        if event in ("peer_closed", "peer_reset"):
            return "yes" if delta <= 1.0 else "slow"
        return "no" if event == "timeout_still_open" else "unknown"
    if event == "write_failed":
        return "yes" if delta <= 3.0 else "slow"
    return "no" if event == "completed" else "unknown"


summary = {}
print(f"{'path':16}{'i':>2}{'rc':>4}  {'backend_event':22}{'delta_s':>8}  result")
for peer, key in sorted(requests.items(), key=lambda kv: kv[1]):
    c, t = client.get(key), terminal.get(peer)
    if not c or not t:
        print(f"{key[0]:16}{key[1]:>2}  missing data")
        summary.setdefault(key[0], Counter())["missing"] += 1
        continue
    delta = t["t"] - c["curl_end"]
    r = result(key[0], t["event"], delta)
    summary.setdefault(key[0], Counter())[r] += 1
    print(f"{key[0]:16}{key[1]:>2}{c['rc']:>4}  {t['event']:22}{delta:>+8.2f}  {r}")

print()
for path in ("hold-neg1", "hold-default", "stream-neg1", "stream-default"):
    counts = summary.get(path, Counter())
    print(f"{path:16}" + "  ".join(f"{k}={counts[k]}" for k in ("yes", "slow", "no", "unknown", "missing")))
