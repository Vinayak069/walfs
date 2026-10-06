#!/usr/bin/env python3
"""Turn the fio JSON files written by bench.sh into a Markdown comparison table."""
import json, pathlib, sys

root = pathlib.Path(sys.argv[1])
labels = sorted(p.name for p in root.iterdir() if p.is_dir())
order = ["walfs-ordered", "walfs-ordered-snap", "walfs-data", "ext4", "tmpfs"]
labels = [l for l in order if l in labels] + [l for l in labels if l not in order]
jobs = ["seq_write", "seq_read", "rand_read", "rand_write", "fsync_write", "fsync_append", "small_files"]


def stat(label, job):
    p = root / label / f"{job}.json"
    if not p.exists():
        return None
    j = json.loads(p.read_text())["jobs"][0]
    r, w = j["read"], j["write"]
    d = w if w["io_bytes"] else r
    lat = d.get("clat_ns", {}).get("percentile", {})
    return {
        "mbps": d["bw_bytes"] / 1e6, "iops": d["iops"],
        "p50": lat.get("50.000000", 0) / 1e3, "p99": lat.get("99.000000", 0) / 1e3,
    }


print("| workload | metric | " + " | ".join(labels) + " |")
print("|---|---|" + "---|" * len(labels))
for job in jobs:
    rows = {l: stat(l, job) for l in labels}
    for metric, fmt in (("mbps", "{:.0f} MB/s"), ("iops", "{:.0f} IOPS"), ("p99", "{:.0f} µs p99")):
        if metric == "iops" and job in ("seq_write", "seq_read"):
            continue
        if metric == "p99" and job in ("seq_write", "seq_read", "small_files"):
            continue
        cells = [fmt.format(rows[l][metric]) if rows[l] else "n/a" for l in labels]
        print(f"| {job} | {metric} | " + " | ".join(cells) + " |")
