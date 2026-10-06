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

## Snapshots

walfs supports up to 32 **copy-on-write snapshots**: instant, read-only, point-in-time
views of the whole filesystem, created and deleted while mounted.

```
mkdir  MOUNT/.snapshots/before-upgrade     # create   (hidden directory, like ZFS's .zfs)
ls     MOUNT/.snapshots/before-upgrade     # browse   (read-only)
rmdir  MOUNT/.snapshots/before-upgrade     # delete
```

### How it works (block level, O(bitmap) creation)

* **Capture.** Creating a snapshot commits the journal, then copies the *block allocation
  bitmap* (the "captured set": 128 KiB per 4 GiB). No file data or inodes are copied, so
  creation cost is independent of how much data exists.
* **Preserve before modify.** When a captured block is about to change for the first time
  (an overwritten data block, a directory block, an indirect block, an inode-table block),
  its old contents are first copied to a fresh block `P` and a record `{X → P, mask}` is
  appended to a persistent **COW log**; `mask` is the set of snapshots that see `P` where
  they refer to `X`. Because the copy happens in the same transaction as the change, the
  two are atomic with respect to crashes, and in `ordered` mode the copy is flushed before
  the commit record like any other ordered data.
* **Transfer on free.** A captured block that is *freed* rather than overwritten (unlink,
  truncate) is not released: it stays allocated and a record `{X → X, mask}` hands it to the
  snapshots. Space returns when the last snapshot that needs it is deleted.
* **Reads.** A snapshot is exposed as a *block device translation* (`snapDev`): logical
  block `X` reads from `P` if a record says so, from the live block otherwise (it has not
  changed since). The ordinary engine code (lookup, readdir, read, …) runs unmodified on top
  of it in read-only mode, so a snapshot is a real filesystem, not a special case.
* **Deletion** clears the snapshot's bit from every record and releases records whose mask
  becomes empty. It proceeds in bounded transactions; a half-deleted snapshot is marked
  *deleting*, is invisible, and is finished at the next mount.
* **Never preserved:** the superblock, journal and bitmaps. A view needs none of them, and
  excluding them avoids recursion (preserving a bitmap block would allocate, which modifies
  a bitmap block).
* **Space safety.** Every operation checks up front that enough spare blocks exist for the
  copies it might need, so it fails cleanly with `ENOSPC` instead of half-way. (As on ZFS, a
  nearly full filesystem with snapshots can refuse deletions until a snapshot is removed.)

### A bug the tests caught
Making a preserved copy allocates a block, and allocation can evict unpinned cache entries —
including the buffer that was just fetched for modification and not yet pinned into the
transaction. It was then re-read stale from disk, so data seemed to revert. The randomized
differential test pinned it to an exact operation; the fix is to pin before preserving.

### Verification
* A **deep-copy model**: the model snapshots its whole state at `CreateSnapshot`, and after
  every operation the tests require every snapshot view to equal the model's frozen copy
  while the live tree keeps matching the live model.
* **fsck** checks the snapshot table and capture bitmaps, the COW log (record sanity, no two
  records for the same block and snapshot, unique ownership of every preserved block), that
  every allocated block is owned by exactly one of {live tree, snapshot machinery, preserved
  copy}, and then runs the *complete* filesystem check on every snapshot through its
  translated view (every block a snapshot references must have been allocated at capture
  time and must still exist, unchanged or preserved).
* The **power-failure simulator** includes snapshot create/delete/read in its workload, so
  recovered images must contain exactly the snapshots (with exactly the contents) of some
  operation-boundary model state; a dedicated test crashes at every point of a
  multi-transaction deletion.
* On a real mount, a `kill -9` test creates snapshots while overwriting files, then
  verifies after recovery that every acknowledged snapshot shows the tree as it was.

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
