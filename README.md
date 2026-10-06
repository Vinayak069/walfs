# walfs — a crash-consistent journaling filesystem in Go

walfs is a POSIX filesystem with a **physical write-ahead journal** (ext4/jbd2-style:
descriptor/commit blocks, CRC-chained transactions, revoke records, group commit,
checkpointing, `ordered` and `data` modes), mounted in user space through **FUSE**.
Its distinguishing feature is how it is verified: a **power-failure simulator** that
replays reordered, sector-torn disk states, and an **independent fsck** plus a
**reference model** used as oracles.

```
walfs mkfs -size 4G disk.img
walfs mount -journal ordered /mnt/walfs disk.img     # unprivileged, via fusermount3
walfs fsck disk.img            # offline consistency check
```

## What is implemented

* On-disk format: superblock, inode & block bitmaps, 256-byte checksummed inodes,
  12 direct + single/double/triple indirect blocks (files up to ~4 TiB, sparse),
  ext2-style directories with an in-memory hash index, fast symlinks.
* POSIX operations: create/mknod/mkdir/unlink/rmdir, **rename** (incl. `NOREPLACE` and
  `EXCHANGE`, directory moves, cycle prevention), hard & symbolic links, truncate,
  `fallocate` (allocate / keep-size / punch-hole), `SEEK_DATA/SEEK_HOLE`, `fsync`,
  open-but-unlinked files (persistent orphan list), `statfs`, READDIRPLUS.
* **Copy-on-write snapshots** (up to 32): O(bitmap) creation, read-only browsable views
  under `MOUNT/.snapshots/` (`mkdir` creates, `rmdir` deletes), space-efficient
  preserve-before-modify with transfer-on-free, resumable deletion.
* Journal: commit protocol with two barriers, **pipelined commit** (disk I/O off the
  filesystem lock, group commit), lazy checkpointing, revoke records, idempotent recovery,
  wrap-around circular log, bounded multi-transaction truncates.
* Verification (see [docs/DESIGN.md](docs/DESIGN.md)):
  differential testing against a model, a CrashMonkey-style crash simulator
  (including crashes *during recovery*), negative controls, ENOSPC testing, and a
  real-mount `kill -9` test.

## Verification results (CI)

| check | what it proves |
|---|---|
| differential tests (3 journal modes, large directories, tiny caches, remounts) | engine ≡ model on random op sequences; fsck clean |
| crash simulator, `ordered` and `data` | every simulated power loss recovers to an exact operation-boundary state no older than the last fsync; recovery is itself crash-safe |
| negative controls (no journal; ignored flushes) | the simulator *does* detect broken designs |
| `kill -9` of the live FUSE daemon, 25 rounds ordered + 15 rounds data | all 8 448 fsync-acknowledged files intact after recovery; fsck clean every round |
| snapshots: deep-copy model comparison after every operation, snapshot-aware fsck, crash simulation incl. mid-deletion, `kill -9` with snapshots | frozen views stay exactly frozen; no leaked or doubly owned blocks; crash-safe creation and deletion |
| asynchronous (pipelined) commit: differential and concurrency stress tests under the **race detector**, power-failure simulation with background commits | the pipeline is as crash-safe as the synchronous commit; no data races |
| race detector run, `gofmt`, `go vet` | |

The simulator found a **real recovery bug** during development (freed directory/symlink
blocks were not revoked, so recovery could replay a stale directory image over reused
file data); see the design doc.

## Performance

Through FUSE on shared CI runners (details, raw data and caveats in
[docs/BENCHMARKS.md](docs/BENCHMARKS.md)):

* durable workloads are on par with ext4: **177–202 MB/s** sequential write with fsync
  (ext4: 203), **2.2–2.8 k** fsync-durable 4 KiB writes/s (ext4: 2.3–2.5 k), small-file
  creation **1.6× faster** than ext4 (group commit);
* random 4 KiB writes reach the hardware ceiling of the runner's disk (~9.4 k IOPS vs a
  measured raw-disk ceiling of ~6–10 k); on RAM-backed storage they run at **118.7 k IOPS**,
  2.4× the previous synchronous commit and above ext4 on a loop device;
* snapshot creation takes **3 ms** on a 1.5 GiB filesystem (O(bitmap)); overwriting
  snapshotted data costs −6…−25 % throughput once per block (copy-on-write).

Profiling under load found and fixed: commit I/O under the global lock (now a pipelined
commit), a per-commit 1 MiB allocation (fsync ×2.2), and a quadratic cache-eviction scan
(data-journal sequential write 22 → 93 MB/s).

## Observability and deployment

`walfs mount -metrics :9100` serves Prometheus metrics (`/metrics`: commit-latency
histogram, journal fill, free space, dirty data, cache size, op/byte counters) and a
`/healthz` probe that turns unhealthy if the filesystem latches read-only after an I/O
error. A `Dockerfile` and `deploy/k8s/walfs.yaml` (PVC-backed Deployment with probes and
scrape annotations) are exercised in CI: the image is run with `/dev/fuse`, written
through, scraped, restarted with data intact, and deployed to a kind cluster.

**Known limitation:** go-fuse does not forward `SYNCFS`, so `sync(2)` does not force a
journal commit; durability is bounded by `fsync` or the `-commit` interval (default 5 s,
like ext4).

## Layout

```
cmd/walfs            CLI (mkfs / mount / fsck / info)
internal/format      on-disk structures, journal scan + recovery, mkfs
internal/engine      the filesystem: inodes, directories, allocation, transactions
internal/fusefs      FUSE adapter (go-fuse low-level API)
internal/blockdev    file / memory / recording / fault-injecting devices
internal/fsck        independent offline checker
internal/model       in-memory reference filesystem (test oracle)
internal/crashtest   workload generator, differential runner, power-failure simulator
scripts, .github     fio suite, kill -9 test, CI workflows
```

## Build & test

```
go build ./cmd/walfs
go test ./...                    # full suite (30+ minutes and memory heavy: runs on CI)
go test -short ./...             # quick pass
scripts/kill9_test.sh ./walfs 25 ordered
```
