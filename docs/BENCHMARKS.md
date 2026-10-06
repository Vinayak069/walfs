# Benchmarks

Everything here was measured on GitHub-hosted `ubuntu-latest` runners (4 vCPU, network
disk) by the workflows in `.github/workflows` (`bench`, `ab`, `perf`); nothing runs on a
developer machine. Raw data: [`results/`](results/). All numbers are single runs on shared
VMs, so expect noise of 5–15 %; comparisons between configurations are therefore made
**back to back on the same runner** (`ab` workflow), not across runs.

walfs runs in user space behind FUSE, so every request that reaches the daemon crosses the
kernel/user boundary twice. Workloads the kernel can answer from its page cache never reach
it. Keep that in mind when reading the ext4 and tmpfs columns.

## 1. Suite on the runner's disk (run `37538250521`)

Image on the runner's disk (walfs: 4 GiB image file via FUSE; ext4: loop-mounted image on
the same disk). Buffered I/O, `end_fsync=1`, `rand_*` = 4 jobs × 256 MiB × 20 s.

| workload | walfs ordered | walfs ordered + snapshot active | walfs data | ext4 (loop) | tmpfs |
|---|---|---|---|---|---|
| seq write 1 GiB + fsync | 177 MB/s | 202 MB/s | 93 MB/s | 203 MB/s | 6711 MB/s |
| seq read (cold) | 2045 MB/s | 2073 MB/s | 1959 MB/s | 7405 MB/s | 10527 MB/s |
| rand read 4 KiB (cold) | 36.2 k IOPS | 36.5 k | 32.6 k | 83.9 k | 1031 k |
| rand write 4 KiB | 9.4 k IOPS | 9.4 k | 8.0 k | 63.5 k | 844 k |
| 4 KiB write + fsync, per second | 2226 | 2203 | 2377 | 2343 | 490 k |
| 4 KiB append + fsync, per second | 2789 | 2710 | 2737 | 2502 | 495 k |
| 4000 small files ×2 jobs | 3287 IOPS | 3324 | 3394 | 2019 | 95 k |

**Durable workloads are the meaningful comparison**: sequential write with a closing fsync
matches ext4, fsync-per-write is on par with ext4 (2.2–2.8 k/s on both), and small-file
creation is 1.6× faster than ext4 (creates are batched into group commits).

## 2. Why random write is "slow": the disk, not the filesystem

The ext4 and tmpfs random-write numbers measure how fast the kernel can *dirty page-cache
pages*; the data is never forced to disk during the 20 s run. walfs journals and flushes
every transaction. The same write pattern that a commit produces (random 4 KiB writes with a
flush every 8192 writes) run **directly on this runner's disk** (`ab` workflow, run
`37530269741`):

| raw disk | IOPS |
|---|---|
| random 4 KiB write, `fdatasync` after every write | 2485 |
| random 4 KiB write, `fdatasync` every 8192 writes | **6128** |

walfs reaches ~9.4 k random-write IOPS on that disk, i.e. it is **at the hardware ceiling**
for durable random writes. To expose the software ceiling, the same suite was run with the
image on a RAM disk, back to back with the configurations that differ only in the commit
pipeline and kernel write-back cache:

| (IOPS unless noted) | old: sync commit, no write-back | async commit + write-back | ext4 on disk |
|---|---|---|---|
| **RAM-backed image**: random write | 49 577 | **118 725** | – |
| RAM-backed: 4 KiB write + fsync | 19 127 | 17 458 | – |
| RAM-backed: sequential write | 1196 MB/s | 1036 MB/s | – |
| disk-backed: random write | 8330 | 9846 | 55–83 k (cached, see above) |
| disk-backed: fsync write | 2360 | 2696 | 4469 |

So on fast storage the pipelined commit (section 4) is **2.4× faster** on random writes and
beats ext4-on-loop (83 k); on this slow disk everything is capped by the device.

## 3. Snapshot cost (run `37538250521`)

~1.5 GiB of data, then 4 KiB random overwrites of one 512 MiB file that existed when the
snapshot was taken:

| step | result |
|---|---|
| overwrites, no snapshot | 10 652 IOPS |
| **snapshot creation** | **3 ms** |
| overwrites with the snapshot (every first touch copies the old block) | 10 058 IOPS (−6 %), 514 MiB preserved |
| the same overwrites again (blocks already copied) | 10 745 IOPS |
| **snapshot deletion** (516 MiB preserved under it) | 147 ms, 514 MiB returned |

Creation copies a 128 KiB bitmap, independent of data size. Earlier runs measured the
first-pass penalty between −6 % and −25 %; expect run-to-run variance of that order.

## 4. What the profiling found (and fixed)

Profiles come from `/debug/pprof` on the daemon under fio load (`perf` workflow).

| finding | fix | effect |
|---|---|---|
| Commit held the global lock during disk I/O; the daemon was ~15 % busy while writers queued (1000+ s of mutex wait) | **pipelined commit**: freeze the transaction (copy block images) under the lock, write it without the lock, finalize under the lock; kernel write-back cache | random write 49.6 k → 118.7 k IOPS on fast storage |
| `fsync` of one 4 KiB write spent ~40 % of CPU allocating/zeroing a 1 MiB log staging buffer per commit, plus GC | pooled staging buffers | fsync throughput ×2.2 |
| In data-journal mode 98 % of CPU was cache eviction scanning an almost fully *pinned* cache on every allocation (quadratic) | bounded eviction work per call | sequential write 22 → 93 MB/s |
| Reads re-fetched data the kernel already had | `FOPEN_KEEP_CACHE` + explicit data-cache control (the daemon is the only writer) | repeated reads: 0 daemon requests |

Cold 4 KiB random reads are bound by FUSE round trips (~14 µs per request): the kernel
serves cached reads without involving the daemon, but `fio` invalidates the cache before each
job, so this benchmark always measures the cold path.

## 5. Correctness is part of performance work

Making commit asynchronous exposed three issues, all caught by the test suite rather than by
users: a latent durability hole (a freed block's checkpoint image was discarded too early,
which could lose committed data if a checkpoint advanced the log tail before the freeing
transaction committed — it also existed in the old synchronous code on the log-full path), and
two harness assumptions about what counts as one operation. See `docs/DESIGN.md`.
