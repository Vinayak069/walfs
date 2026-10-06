#!/usr/bin/env bash
# Run the fio suite against a directory.  usage: bench.sh LABEL DIR OUTDIR [sync-cmd]
# Every job ends with an fsync (end_fsync=1) so that buffered writes are charged
# for durability, which is what a journaling filesystem is for.
set -euo pipefail
LABEL=$1; DIR=$2; OUT=$(realpath -m "$3")
FIO=${FIO:-fio}
mkdir -p "$OUT/$LABEL" "$DIR/bench"
cd "$DIR/bench"

drop_caches() { sync; echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null 2>&1 || true; }
run() { # name, args...
  local name=$1; shift
  echo "== $LABEL: $name"
  $FIO --name="$name" --directory="$DIR/bench" --output-format=json --output="$OUT/$LABEL/$name.json" \
       --group_reporting --ioengine=psync --randrepeat=0 "$@" >/dev/null
}

run seq_write   --rw=write     --bs=1M --size=1G --numjobs=1 --end_fsync=1
drop_caches
run seq_read    --rw=read      --bs=1M --size=1G --numjobs=1
drop_caches
run rand_read   --rw=randread  --bs=4k --size=512M --numjobs=4 --time_based --runtime=20 --ramp_time=3
run rand_write  --rw=randwrite --bs=4k --size=512M --numjobs=4 --time_based --runtime=20 --ramp_time=3 --end_fsync=1
# One synchronous commit per write: measures the cost of the journal commit path.
run fsync_write --rw=randwrite --bs=4k --size=64M  --numjobs=1 --fsync=1 --time_based --runtime=20 --ramp_time=2
run fsync_append --rw=write    --bs=4k --size=64M  --numjobs=1 --fsync=1 --time_based --runtime=20 --ramp_time=2
# Metadata-heavy: thousands of small files created, written and fsynced at the end.
run small_files --rw=write     --bs=4k --filesize=4k --nrfiles=4000 --openfiles=64 --numjobs=2 --end_fsync=1
rm -rf "$DIR/bench"
