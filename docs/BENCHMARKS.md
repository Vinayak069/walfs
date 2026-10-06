# Benchmarks

Measured with fio 3.x on a GitHub-hosted `ubuntu-latest` runner (workflow `bench`, run
`37484228423`), all four targets backed by the same runner disk: walfs mounted through
FUSE from a 4 GiB image file, ext4 on a loop-mounted 4 GiB image, and tmpfs. Raw fio JSON
is in [`results/fio-json`](results/fio-json); regenerate with
`gh workflow run bench.yml` (nothing runs on a developer machine).

All jobs use the `psync` engine, buffered I/O, and `end_fsync=1`, so buffered writes are
charged for durability. `rand_*` use 4 jobs × 256 MiB, 4 KiB blocks, 20 s; `fsync_*` issue
one `fsync` after every 4 KiB write.

| workload | metric | walfs (ordered) | walfs (data) | ext4 (loop) | tmpfs |
|---|---|---|---|---|---|
| seq write 1 GiB, 1 MiB blocks | throughput | **207 MB/s** | 105 MB/s | 199 MB/s | 7255 MB/s |
| seq read | throughput | 2030 MB/s | 1867 MB/s | 7781 MB/s | 10527 MB/s |
| rand read 4 KiB ×4 | IOPS | 33.3 k | 32.8 k | 84.5 k | 1045 k |
| rand write 4 KiB ×4 | IOPS | 7.6 k | 7.1 k | 76.7 k | 888 k |
| 4 KiB write + fsync (random) | fsyncs/s | 1367 | 1944 | 2653 | 472 k |
| 4 KiB append + fsync | fsyncs/s | 1780 | 2231 | 2935 | 491 k |
| 4000 × 4 KiB small files ×2 | IOPS | **3854** | 3964 | 2138 | 96 k |

## How to read this honestly

* **walfs runs in user space behind FUSE**; every request crosses the kernel↔daemon
  boundary twice. That, not the on-disk design, dominates the gaps on random I/O and on
  cached reads, and it is why walfs cannot approach tmpfs/ext4 there. Numbers are
  best compared *between the two walfs journal modes* and against ext4 for the
  durability-bound workloads.
* **Durable workloads are the meaningful comparison.** Sequential write with a final
  fsync matches ext4 (207 vs 199 MB/s) and fsync-per-write reaches 51–76 % of ext4's
  rate (journal commit = log write + barrier + commit write + barrier).
* Small-file creation beats ext4 on this setup because creates are batched into group
  commits and metadata is written to the log sequentially.
* `data` journaling halves sequential write throughput (data is written twice) but is
  slightly *faster* for fsync-heavy small writes: one log append instead of data +
  journal.
* ext4's sequential-read figure is page-cache bound (`drop_caches` is best effort on a
  shared runner), so it should not be read as disk speed.
* Single run, shared VM: expect noise of several percent. The p99 latencies in the raw
  JSON are completion latencies of the write syscall, not of the fsync.
