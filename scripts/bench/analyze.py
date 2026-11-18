#!/usr/bin/env python3
"""Summarize a fam run log: wall time, remote command time, and the slowest commands."""
import collections
import json
import sys
from datetime import datetime


def ts(s):
    s = s.rstrip("Z")
    if "." in s:
        a, b = s.split(".")
        s = a + "." + b[:6]
    return datetime.fromisoformat(s)


path = sys.argv[1]
top = int(sys.argv[2]) if len(sys.argv) > 2 else 15
cmds, result = [], None
for line in open(path):
    d = json.loads(line)
    if d["type"] == "command":
        cmds.append(d)
    elif d["type"] == "result":
        result = d["result"]

print(f"run      {result['runId']}  duration {result['duration']}")
first = ts(cmds[0]["time"]) if cmds else None
last = ts(cmds[-1]["time"]) if cmds else None
total = sum(c.get("durationMs", 0) for c in cmds)
print(f"commands {len(cmds)}  sum {total/1000:.1f}s")
if cmds:
    print(f"last command ends at +{(last - ts(result['started'])).total_seconds():.1f}s")
    print(f"first command ends at +{(first - ts(result['started'])).total_seconds():.1f}s after start")
by = collections.defaultdict(lambda: [0, 0])
for c in cmds:
    lab = c.get("label", "")
    key = lab.split(" ")[0] if lab else "?"
    by[key][0] += 1
    by[key][1] += c.get("durationMs", 0)
print("\nby label verb:")
for k, (n, ms) in sorted(by.items(), key=lambda kv: -kv[1][1])[:20]:
    print(f"  {k:<14} n={n:<4} {ms/1000:7.1f}s")
print(f"\nslowest {top}:")
for c in sorted(cmds, key=lambda c: -c.get("durationMs", 0))[:top]:
    print(f"  {c.get('durationMs',0)/1000:7.2f}s  {c.get('label','')[:90]}")
# Idle gaps: time when no command ran (local work: Pkl, GitHub, etc.). The log writes each
# entry when the command ends, so a command ran from time - duration to time.
spans = sorted((ts(c["time"]).timestamp() - c.get("durationMs", 0) / 1000, ts(c["time"]).timestamp(), c.get("label", "")) for c in cmds)
gaps = []
end, end_label = None, ""
for start, stop, label in spans:
    if end is not None and start > end:
        gaps.append((start - end, end_label, label))
    if end is None or stop > end:
        end, end_label = stop, label
print("\nlargest idle gaps (no command running):")
for g, a, b in sorted(gaps, reverse=True)[:8]:
    print(f"  {g:6.2f}s  after [{a[:50]}] before [{b[:50]}]")
