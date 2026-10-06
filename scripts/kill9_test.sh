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
def note(line):                      # durable bookkeeping on the host filesystem
    a.write(line + "\n"); a.flush(); os.fsync(a.fileno())
def write_all(fd, data):
    mv = memoryview(data)
    while mv:                        # write(2) may be short (e.g. near ENOSPC)
        mv = mv[os.write(fd, mv):]
sha = lambda b: hashlib.sha256(b).hexdigest()
i = snapn = 0
try:
    snapdir = os.path.join(mnt, ".snapshots")
    for name in os.listdir(snapdir):             # clear snapshots left by earlier rounds
        note(f"SD {name}")
        os.rmdir(os.path.join(snapdir, name))
        note(f"SNAPDEL {name}")
    while True:
        data = rng.randbytes(rng.choice([100, 5000, 70000, 400000]))
        p = os.path.join(d, f"f{i}")
        fd = os.open(p, os.O_CREAT | os.O_WRONLY, 0o644)
        write_all(fd, data)
        os.fsync(fd)
        os.close(fd)
        dfd = os.open(d, os.O_RDONLY); os.fsync(dfd); os.close(dfd)
        note(f"F {p[len(mnt):]} {sha(data)}")      # acknowledged: must survive
        i += 1
        if i % 7 == 0:               # also churn the namespace
            os.rename(p, p + ".r"); os.rename(p + ".r", p)
        if i % 20 == 0:
            # Take a snapshot, then overwrite f0 in place so that the
            # copy-on-write path runs while the snapshot exists.
            name = f"r{rnd}_s{snapn}"; snapn += 1
            f0 = os.path.join(d, "f0")
            old = sha(open(f0, "rb").read())
            os.mkdir(os.path.join(mnt, ".snapshots", name))
            nd = rng.randbytes(30000)
            note(f"M {f0[len(mnt):]} {sha(nd)}")  # may or may not be applied if we die now
            fd0 = os.open(f0, os.O_WRONLY)
            write_all(fd0, nd); os.ftruncate(fd0, 30000); os.fsync(fd0); os.close(fd0)
            # That fsync also committed the snapshot creation (one total order).
            note(f"F {f0[len(mnt):]} {sha(nd)}")
            note(f"SNAP {name} round{rnd} {i} {old}")
            if snapn % 2 == 0:       # keep the number of snapshots bounded
                victim = f"r{rnd}_s{snapn-2}"
                note(f"SD {victim}")             # may or may not be deleted if we die now
                os.rmdir(os.path.join(mnt, ".snapshots", victim))
                dfd = os.open(d, os.O_RDONLY); os.fsync(dfd); os.close(dfd)
                note(f"SNAPDEL {victim}")
except OSError:
    pass                             # the daemon was killed under us
PY

cat > "$W/verify.py" <<'PY'
import os, sys, hashlib
mnt, ack = sys.argv[1], sys.argv[2]
sha = lambda b: hashlib.sha256(b).hexdigest()
allowed = {}          # path -> hashes the file may legitimately have
snaps, maybe_deleted = {}, set()
for line in open(ack):
    t = line.split()
    if t[0] == "F":                       # acknowledged: exactly this from now on
        allowed[t[1]] = {t[2]}
    elif t[0] == "M":                     # in flight: old or new
        allowed.setdefault(t[1], set()).add(t[2])
    elif t[0] == "SNAP":
        snaps[t[1]] = (t[2], int(t[3]), t[4])
    elif t[0] == "SD":
        maybe_deleted.add(t[1])
    elif t[0] == "SNAPDEL":
        snaps.pop(t[1], None)
        maybe_deleted.discard(t[1])
bad = 0
def read(path):
    try:
        return open(path, "rb").read()
    except OSError:
        return None
for path, hashes in allowed.items():
    got = read(mnt + path)
    if got is None:
        print(f"LOST {path}"); bad += 1
    elif sha(got) not in hashes:
        print(f"CORRUPT {path}: {len(got)} bytes, hash matches none of {len(hashes)} acceptable versions"); bad += 1
# Every acknowledged snapshot must exist (unless a deletion was in flight) and
# show the tree as it was: all files created before it, f0 in its old version.
for name, (rdir, count, f0sum) in snaps.items():
    base = f"{mnt}/.snapshots/{name}/{rdir}"
    if not os.path.isdir(base):
        if name not in maybe_deleted:
            print(f"SNAPSHOT {name} lost"); bad += 1
        continue
    g = read(base + "/f0")
    if g is None or sha(g) != f0sum:
        print(f"SNAPSHOT {name}: f0 is not the version from snapshot time"); bad += 1
    for k in range(1, count):
        want = allowed.get(f"/{rdir}/f{k}")
        g = read(f"{base}/f{k}")
        if want is None:
            continue
        if g is None or sha(g) not in want:
            print(f"SNAPSHOT {name}: f{k} missing or wrong"); bad += 1
live = sum(1 for n in snaps if os.path.isdir(f"{mnt}/.snapshots/{n}"))
print(f"verified {len(allowed)} files and {live}/{len(snaps)} acknowledged snapshots")
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
