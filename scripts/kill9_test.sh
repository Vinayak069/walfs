#!/usr/bin/env bash
# Real-mount crash test: kill -9 the FUSE daemon while a writer is fsync-ing
# files, remount (journal recovery), and verify every acknowledged file is
# intact and fsck is clean.   usage: kill9_test.sh WALFS_BINARY [ROUNDS] [MODE]
set -euo pipefail
BIN=$(realpath "$1"); ROUNDS=${2:-20}; MODE=${3:-ordered}
W=$(mktemp -d); IMG=$W/img; MNT=$W/mnt; ACK=$W/ack; mkdir -p "$MNT"
"$BIN" mkfs -size 512M "$IMG" >/dev/null

mount_fs() {
  "$BIN" mount -journal "$MODE" -commit 2s "$MNT" "$IMG" 2>>"$W/daemon.log" &
  DPID=$!
  for _ in $(seq 100); do mountpoint -q "$MNT" && return; sleep 0.1; done
  echo "mount failed"; cat "$W/daemon.log"; exit 1
}

cat > "$W/writer.py" <<'PY'
import os, sys, hashlib, random
mnt, ack, rnd = sys.argv[1], sys.argv[2], int(sys.argv[3])
rng = random.Random(rnd)
d = os.path.join(mnt, f"round{rnd}")
os.makedirs(d, exist_ok=True)
a = open(ack, "a")
i = 0
try:
    while True:
        data = rng.randbytes(rng.choice([100, 5000, 70000, 400000]))
        p = os.path.join(d, f"f{i}")
        fd = os.open(p, os.O_CREAT | os.O_WRONLY, 0o644)
        os.write(fd, data)
        os.fsync(fd)
        os.close(fd)
        dfd = os.open(d, os.O_RDONLY); os.fsync(dfd); os.close(dfd)
        a.write(f"{p[len(mnt):]} {hashlib.sha256(data).hexdigest()}\n"); a.flush(); os.fsync(a.fileno())
        i += 1
        if i % 7 == 0:       # also churn the namespace
            os.rename(p, p + ".r"); os.rename(p + ".r", p)
except OSError:
    pass                       # the daemon was killed under us
PY

total=0
for r in $(seq "$ROUNDS"); do
  mount_fs
  python3 "$W/writer.py" "$MNT" "$ACK" "$r" &
  WPID=$!
  sleep "0.$((RANDOM % 9 + 1))"
  kill -9 "$DPID"; wait "$DPID" 2>/dev/null || true
  wait "$WPID" 2>/dev/null || true
  fusermount3 -u -z "$MNT" 2>/dev/null || true
  sleep 0.2
  # Remount => journal recovery, then verify.
  mount_fs
  bad=0; n=0
  while read -r path sum; do
    n=$((n+1))
    got=$(sha256sum "$MNT$path" 2>/dev/null | cut -d' ' -f1 || true)
    if [ "$got" != "$sum" ]; then echo "ROUND $r: $path lost or corrupt (want $sum got $got)"; bad=$((bad+1)); fi
  done < "$ACK"
  total=$n
  fusermount3 -u "$MNT"; wait "$DPID" 2>/dev/null || true
  "$BIN" fsck "$IMG" >/dev/null || { "$BIN" fsck "$IMG"; exit 1; }
  [ "$bad" -eq 0 ] || { echo "FAILED: $bad acknowledged files missing/corrupt"; exit 1; }
  echo "round $r ok: $n acknowledged files verified after kill -9 + recovery"
done
echo "PASS: $ROUNDS kill -9 rounds, $total acknowledged-durable files, fsck clean every time"
grep -h "recovery" "$W/daemon.log" | head -5 || true
