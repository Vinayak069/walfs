#!/usr/bin/env python3
import json, pathlib, sys
out = pathlib.Path(sys.argv[1])
cfgs = ["sync-nowb", "sync-wb", "async-nowb", "async-wb", "ram-sync-nowb", "ram-async-wb", "ext4", "tmpfs"]
jobs = ["rand_write", "rand_read", "fsync_write", "seq_write"]
def get(c, j):
    p = out / f"{c}.{j}.json"
    if not p.exists():
        return "n/a"
    d = json.loads(p.read_text())["jobs"][0]
    x = d["write"] if d["write"]["io_bytes"] else d["read"]
    if j == "seq_write":
        return f"{x['bw_bytes']/1e6:.0f} MB/s"
    return f"{x['iops']:.0f}"
print("| workload | " + " | ".join(cfgs) + " |")
print("|---|" + "---|" * len(cfgs))
for j in jobs:
    print(f"| {j} | " + " | ".join(get(c, j) for c in cfgs) + " |")
