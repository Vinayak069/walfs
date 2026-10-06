#!/usr/bin/env bash
# A/B comparison of walfs mount configurations (and ext4 for reference) on one
# machine, back to back, so runner-to-runner noise does not mask differences.
# usage: ab.sh WALFS_BINARY OUTDIR
set -euo pipefail
BIN=$(realpath "$1"); OUT=$(realpath -m "$2"); mkdir -p "$OUT"
RUNTIME=${RUNTIME:-15}

job() { # dir name fio-args...
  local dir=$1 name=$2; shift 2
  fio --name=$name --directory=$dir --output-format=json --output="$OUT/$CFG.$name.json" \
      --group_reporting --ioengine=psync --randrepeat=0 --time_based --runtime=$RUNTIME --ramp_time=2 \
      "$@" >/dev/null
}
suite() { # dir
  local dir=$1
  fio --name=layout --directory=$dir --rw=write --bs=1M --size=256M --numjobs=4 --end_fsync=1 >/dev/null
  job $dir rand_write  --rw=randwrite --bs=4k --size=256M --numjobs=4 --filename_format='layout.$jobnum.0'
  job $dir rand_read   --rw=randread  --bs=4k --size=256M --numjobs=4 --filename_format='layout.$jobnum.0'
  job $dir fsync_write --rw=randwrite --bs=4k --size=64M  --numjobs=1 --fsync=1 --filename_format='layout.0.0'
  job $dir seq_write   --rw=write     --bs=1M --size=512M --numjobs=1 --end_fsync=1 --filename_format='seq.$jobnum' --time_based=0 --runtime=0 --ramp_time=0
}

for cfg in "sync-nowb:-async-commit=false -writeback=false" "sync-wb:-async-commit=false -writeback=true" \
           "async-nowb:-async-commit=true -writeback=false" "async-wb:-async-commit=true -writeback=true"; do
  CFG=${cfg%%:*}; ARGS=${cfg#*:}
  rm -f /tmp/ab.img; "$BIN" mkfs -size 4G /tmp/ab.img >/dev/null
  mkdir -p /tmp/abm; "$BIN" mount $ARGS /tmp/abm /tmp/ab.img 2>"$OUT/$CFG.log" &
  DPID=$!; sleep 2
  echo "== $CFG ($ARGS)"
  suite /tmp/abm
  fusermount3 -u /tmp/abm; wait $DPID || true
  "$BIN" fsck /tmp/ab.img >/dev/null
done

# Software ceiling: the same suite with the image on a RAM disk, so the storage
# device cannot be the limit.
for cfg in "ram-sync-nowb:-async-commit=false -writeback=false" "ram-async-wb:-async-commit=true -writeback=true"; do
  CFG=${cfg%%:*}; ARGS=${cfg#*:}
  rm -f /dev/shm/ab.img; "$BIN" mkfs -size 3G /dev/shm/ab.img >/dev/null
  mkdir -p /tmp/abm; "$BIN" mount $ARGS /tmp/abm /dev/shm/ab.img 2>"$OUT/$CFG.log" &
  DPID=$!; sleep 2
  echo "== $CFG ($ARGS)"
  suite /tmp/abm
  fusermount3 -u /tmp/abm; wait $DPID || true
  rm -f /dev/shm/ab.img
done
CFG=tmpfs
mkdir -p /tmp/tmpfsm; sudo mount -t tmpfs -o size=3g tmpfs /tmp/tmpfsm; sudo chown $USER /tmp/tmpfsm
echo "== tmpfs"
suite /tmp/tmpfsm
sudo umount /tmp/tmpfsm

CFG=ext4
truncate -s 4G /tmp/ext4.img; mkfs.ext4 -q -F /tmp/ext4.img
mkdir -p /tmp/ext4m; sudo mount -o loop /tmp/ext4.img /tmp/ext4m; sudo chown $USER /tmp/ext4m
echo "== ext4"
suite /tmp/ext4m
sudo umount /tmp/ext4m
