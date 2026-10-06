#!/usr/bin/env bash
# Real-mount crash test: kill -9 the FUSE daemon while a writer is fsync-ing
# files, remount (journal recovery), and verify every acknowledged file is
# intact and fsck is clean.   usage: kill9_test.sh WALFS_BINARY [ROUNDS] [MODE]
set -euo pipefail
BIN=$(realpath "$1"); ROUNDS=${2:-20}; MODE=${3:-ordered}
W=$(mktemp -d); IMG=$W/img; MNT=$W/mnt; ACK=$W/ack; mkdir -p "$MNT"
"$BIN" mkfs -size 3G "$IMG" >/dev/null

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
        mv = memoryview(data)
        while mv:                      # write(2) may be short (e.g. near ENOSPC)
            mv = mv[os.write(fd, mv):]
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

cat > "$W/verify.py" <<'PY'
import os, sys, hashlib, random
mnt, ack = sys.argv[1], sys.argv[2]
bad = 0
cache = {}
def expected(rnd, idx):
    # Regenerate exactly what writer.py wrote for file idx of round rnd.
    if rnd not in cache:
        cache[rnd] = (random.Random(rnd), [])
    rng, seq = cache[rnd]
    while len(seq) <= idx:
        seq.append(rng.randbytes(rng.choice([100, 5000, 70000, 400000])))
    return seq[idx]
for line in open(ack):
    path, want = line.split()
    try:
        got = open(mnt + path, "rb").read()
    except OSError as e:
        print(f"LOST {path}: {e}"); bad += 1; continue
    if hashlib.sha256(got).hexdigest() == want:
        continue
    bad += 1
    rnd = int(path.split("/")[1][5:]); idx = int(path.rsplit("/f", 1)[1])
    exp = expected(rnd, idx)
    first = next((i for i in range(min(len(got), len(exp))) if got[i] != exp[i]), None)
    diff = [i for i in range(min(len(got), len(exp))) if got[i] != exp[i]]
    zero = sum(1 for i in diff if got[i] == 0)
    print(f"CORRUPT {path}: want {len(exp)} bytes, got {len(got)}; first diff at {first}; "
          f"{len(diff)} differing bytes ({zero} are zero in the file); "
          f"diff range {diff[0] if diff else None}..{diff[-1] if diff else None}")
    print(f"  expected bytes at first diff: {exp[first:first+8].hex() if first is not None else '-'}, "
          f"found: {got[first:first+8].hex() if first is not None else '-'}")
sys.exit(1 if bad else 0)
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
  n=$(wc -l < "$ACK"); bad=0
  python3 "$W/verify.py" "$MNT" "$ACK" || bad=1
  total=$n
  fusermount3 -u "$MNT"; wait "$DPID" 2>/dev/null || true
  "$BIN" fsck "$IMG" >/dev/null || { "$BIN" fsck "$IMG"; exit 1; }
  [ "$bad" -eq 0 ] || { echo "FAILED round $r: acknowledged files missing/corrupt"; cat "$W/daemon.log" | tail -20; exit 1; }
  echo "round $r ok: $n acknowledged files verified after kill -9 + recovery"
done
echo "PASS: $ROUNDS kill -9 rounds, $total acknowledged-durable files, fsck clean every time"
grep -h "recovery" "$W/daemon.log" | head -5 || true
