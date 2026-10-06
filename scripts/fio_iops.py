#!/usr/bin/env python3
import json, sys
j = json.load(open(sys.argv[1]))["jobs"][0]["write"]
print(f"{j['iops']:.0f} IOPS, {j['bw_bytes']/1e6:.1f} MB/s")
