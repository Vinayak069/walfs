# walfs design

walfs is a POSIX filesystem implemented in Go, exposed through FUSE, built around a
**physical write-ahead journal** in the style of ext4/jbd2. It is a vehicle for
getting the hard parts of a filesystem *right and provably so*: crash consistency,
allocation, directory structure, and verification.

```
 applications ──syscalls──▶ kernel VFS ──▶ /dev/fuse ──▶ internal/fusefs   (translation only)
                                                              │
                                                       internal/engine      inodes, dirs, allocation,
                                                              │             file I/O, transactions
                         ┌────────────────────────────────────┼───────────────────┐
                    internal/format                    internal/blockdev     internal/fsck
                    on-disk structures,                file / memory /       independent checker
                    journal scan + recovery            recording devices
```

## On-disk layout (4 KiB blocks)

| region | contents |
|---|---|
| block 0 | superblock (one 512 B sector, CRC32C) |
| journal | journal superblock + circular log |
| inode bitmap, block bitmap | one bit per inode / block |
| inode table | 256-byte inodes (CRC32C seeded with the inode number) |
| data area | file data, directory blocks, indirect blocks |

* **Inodes**: mode, uid/gid, nlink, size, 4 timestamps (ns), generation, 12 direct +
  single/double/triple indirect pointers (max file ≈ 4 TiB), sparse files, "fast"
  symlinks stored in the pointer area.
* **Directories**: ext2-style variable-length entries chained by `rec_len`, plus an
  in-memory name→inode index per directory (rebuilt lazily after mount) so lookups are O(1).
* **Allocator**: bitmaps with per-bitmap-block free counters, goal-directed
  (next to the file's previous block) so sequential writes become sequential extents.
  A small reserved pool lets metadata operations succeed on a "full" disk.

## The journal

Every metadata update is a *transaction* of whole-block images:

```
[REVOKE]* ( [DESCRIPTOR] [block image]{n} )* [COMMIT]
```

* Each block carries the transaction sequence number; the **COMMIT** block holds a
  CRC32C chained over every preceding block (seeded with the filesystem UUID and the
  sequence number). A transaction is valid iff its commit block matches. This single rule
  makes recovery independent of the order in which a disk persists the individual writes.
* **Commit protocol**: write data (ordered mode) → write log blocks → *barrier* →
  write commit block → *barrier*.
* **Checkpoint**: committed blocks are written to their home locations lazily (hot
  blocks such as bitmaps are written once per checkpoint, not once per transaction); only
  after that write is durable is the journal tail advanced. Log offsets are monotonically
  increasing logical counters, so a full log is never confused with an empty one.
* **Revoke records**: when a block that was journaled is freed (and may be reused as file
  data, which is written in place) a revoke record stops recovery from replaying the stale
  journal image over the new contents. *Directory and symlink blocks count as metadata too*
  — the crash simulator found a bug where freeing them without a revoke let recovery
  corrupt file data (see "Bugs found by verification").
* **Freed blocks are not reusable until the freeing transaction commits.** Otherwise an
  in-place data write could land on a block that the last committed state still treats as
  metadata.
* **Recovery**: scan from the tail, validate each transaction, replay up to the first
  invalid one, then scrub the log head and persist the new tail. It is idempotent: a crash
  *during* recovery is handled by simply running it again (tested exhaustively).

### Journal modes

| mode | what is journaled | guarantee |
|---|---|---|
| `ordered` (default) | metadata; file data is written in place *before* the commit that references it | every operation is atomic; no stale/uninitialised data is ever exposed; in-place overwrites may be old or new after a crash (same as ext4 `data=ordered`) |
| `data` | metadata and data | every operation (including overwrites of ≤ 15 blocks) is atomic; data is written twice |
| `none` | nothing (ext2-like) | baseline and negative control — *not* crash safe |

### Operations are atomic; commits are group commits

All mutating operations run under one lock and join the single running transaction, so a
transaction always contains a *prefix of whole operations*. Commit happens on `fsync`,
when the transaction fills, when too much data is dirty, or every 5 s (configurable).
`fsync` therefore makes *everything issued so far* durable.

Unlinked-but-open files live on a persistent **orphan list** so that space is reclaimed
after a crash; large deletes/truncates proceed in bounded steps, each a consistent state.

## Verification

Correctness is established by testing against independent oracles rather than by trust:

1. **`internal/fsck`** re-implements block-map and directory parsing from the format
   package only. It checks checksums, block ownership (no double allocation), bitmap ⇔
   reachability equality, link counts, `.`/`..`, directory tree shape (no cycles, one
   parent), the orphan list, and `i_blocks`.
2. **`internal/model`** is a ~500-line in-memory reference filesystem. **Differential
   tests** apply identical random operations (create, write, truncate, rename incl.
   `RENAME_EXCHANGE`/`NOREPLACE`, link, symlink, fallocate/punch-hole, open-unlinked
   files, …) to the engine and the model and require identical results, identical trees
   and clean fsck, with tiny caches to force eviction and periodic remounts.
3. **Power-failure simulator** (`internal/crashtest`): a recording block device logs every
   write and flush. For *every* crash point it reconstructs the disk a real device could
   present: all flushed writes plus **a random subset of the unflushed writes, reordered
   and torn at 512-byte sector granularity**. Each image is mounted (recovering the
   journal), fscked, and its tree must equal **exactly one operation-boundary state of
   the model that is no older than the last completed `fsync`**. A fraction of images
   additionally crash *during recovery* at every write. Negative controls (no journal;
   a device that ignores flushes) must be detected as broken, proving the harness has teeth.

### Bugs found by verification
* Freed directory/symlink blocks were not revoked, so crash recovery could replay a stale
  directory image over reused file data. (Found by the crash simulator; fixed by
  classifying freed blocks by inode type.)
* Test-harness bugs that the oracles exposed in themselves (e.g. an "append-only"
  workload that silently overwrote through handles).
