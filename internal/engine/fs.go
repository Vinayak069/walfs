// Package engine implements the walfs filesystem proper: a Unix-style inode
// filesystem (inode table, bitmaps, direct/indirect block maps, ext2-style
// directories) whose every metadata update goes through a physical
// write-ahead journal with revoke records, ordered or full data journaling,
// group commit and checkpointing.
//
// The engine is independent of FUSE: it exposes a small VFS-like API keyed by
// inode numbers, which both the FUSE adapter and the crash-test harness drive.
package engine

import (
	"errors"
	"fmt"
	"math/bits"
	"sync"
	"syscall"
	"time"

	"github.com/vinayak-singh/walfs/internal/blockdev"
	"github.com/vinayak-singh/walfs/internal/format"
)

// JournalMode selects what is written through the journal.
type JournalMode int

const (
	// ModeOrdered journals metadata only. File data is written to its home
	// location before the transaction that makes it reachable commits, so a
	// crash can never expose uninitialised or stale blocks through a file.
	// (Same guarantee as ext4 data=ordered.)
	ModeOrdered JournalMode = iota
	// ModeData journals data as well as metadata: every operation is atomic,
	// including in-place overwrites, at the price of writing data twice.
	ModeData
	// ModeNone disables the journal entirely. Dirty blocks are simply written
	// back at sync time with no ordering guarantees (ext2-like). It exists as
	// a benchmark baseline and as a negative control for the crash tester.
	ModeNone
)

func (m JournalMode) String() string {
	switch m {
	case ModeOrdered:
		return "ordered"
	case ModeData:
		return "data"
	case ModeNone:
		return "none"
	}
	return "?"
}

// ParseMode parses "ordered", "data" or "none".
func ParseMode(s string) (JournalMode, error) {
	switch s {
	case "ordered", "":
		return ModeOrdered, nil
	case "data":
		return ModeData, nil
	case "none", "off":
		return ModeNone, nil
	}
	return 0, errors.New("journal mode must be ordered, data or none")
}

// Options configures a mounted filesystem.
type Options struct {
	Mode             JournalMode
	CommitInterval   time.Duration // background commit period; 0 disables the timer
	CacheBlocks      int           // buffer cache capacity (default 32768 = 128 MiB)
	DirtyLimitBlocks int           // commit when this many data blocks are dirty (default 8192 = 32 MiB)
	Clock            func() int64  // nanosecond clock, injectable for tests; must be safe for concurrent use (background commits call it)
	ReadOnly         bool
	// AsyncCommit lets commits run in the background so that operations
	// continue while a transaction is being written (pipelined commit). With
	// it off, every commit completes before the triggering call returns,
	// which makes event order deterministic (the crash tests rely on that).
	AsyncCommit  bool
	SkipRecovery bool // fsck/diagnostics: do not replay the journal
}

func (o *Options) fill() {
	if o.CacheBlocks == 0 {
		o.CacheBlocks = 32768
	}
	if o.DirtyLimitBlocks == 0 {
		o.DirtyLimitBlocks = 8192
	}
	if o.Clock == nil {
		o.Clock = func() int64 { return time.Now().UnixNano() }
	}
}

// Stats are cumulative counters, useful for benchmarking and tests.
type Stats struct {
	Commits         uint64
	Checkpoints     uint64
	Flushes         uint64
	JournalBlocks   uint64 // blocks written to the log (descriptors, data, commits)
	HomeMetaBlocks  uint64 // metadata blocks written at checkpoint
	DataBlocks      uint64 // ordered data blocks written at commit
	Revoked         uint64
	SnapPreserved   uint64 // blocks copied because a snapshot still saw them
	SnapTransferred uint64 // freed blocks handed over to snapshots

	// Commit latency histogram: CommitBuckets[i] counts commits that took at
	// most CommitBounds[i] (cumulative form is derived when exported).
	CommitBuckets [len(CommitBounds) + 1]uint64
	CommitNanos   uint64
}

// CommitBounds are the upper bounds of the commit-latency histogram.
var CommitBounds = [...]time.Duration{
	100 * time.Microsecond, 250 * time.Microsecond, 500 * time.Microsecond,
	time.Millisecond, 2500 * time.Microsecond, 5 * time.Millisecond,
	10 * time.Millisecond, 25 * time.Millisecond, 100 * time.Millisecond, time.Second,
}

func (s *Stats) observeCommit(d time.Duration) {
	s.CommitNanos += uint64(d)
	for i, b := range CommitBounds {
		if d <= b {
			s.CommitBuckets[i]++
			return
		}
	}
	s.CommitBuckets[len(CommitBounds)]++
}

// Gauges are point-in-time values for monitoring.
type Gauges struct {
	FreeBlocks, TotalBlocks uint64
	FreeInodes, TotalInodes uint64
	JournalUsed, JournalCap uint64 // blocks
	DirtyDataBlocks         int
	TxnBlocks               int
	CacheBlocks             int
	PendingCheckpoint       int
	ReadOnly                bool
	Snapshots               int
	CowRecords              int
}

// Gauges returns a consistent snapshot of the gauges.
func (fs *FS) Gauges() Gauges {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return Gauges{
		FreeBlocks: fs.freeBlocks, TotalBlocks: fs.sb.TotalBlocks - fs.sb.DataStart,
		FreeInodes: uint64(fs.freeInodes), TotalInodes: uint64(fs.sb.InodeCount) - 1,
		JournalUsed: fs.journalUsed(), JournalCap: fs.jsb.Capacity(),
		DirtyDataBlocks: len(fs.txData), TxnBlocks: fs.txBlocks(),
		CacheBlocks: fs.c.len(), PendingCheckpoint: len(fs.ckptImg),
		ReadOnly:  fs.aborted != nil || fs.opts.ReadOnly,
		Snapshots: bits.OnesCount32(fs.snapMask), CowRecords: fs.cowRecords(),
	}
}

// FS is a mounted walfs.
type FS struct {
	dev  blockdev.Device
	sb   *format.Superblock
	opts Options

	// mu serialises all mutating operations (Lock) and admits concurrent
	// readers (RLock). A commit runs under the write lock, so a transaction
	// always contains whole operations and recovery lands on an operation
	// boundary.
	mu sync.RWMutex
	c  *cache

	// --- running transaction ---
	txMeta       map[uint64]*buf     // journaled blocks (metadata; plus data in ModeData)
	txData       []*buf              // ordered-mode data blocks to write home before commit
	txRevoke     map[uint64]struct{} // freed blocks whose older journal images must not replay
	freedPending map[uint64]struct{} // freed in the running txn: not reusable until it commits
	aborted      error               // set on journal I/O failure: filesystem goes read-only

	// --- journal state ---
	jsb          format.JournalSuper
	headOff      uint64 // logical offset where the next transaction starts
	nextSeq      uint64
	ckptImg      map[uint64][]byte // committed block images awaiting a home-location write
	committing   *commitTx         // commit whose I/O is in flight (nil if none)
	committedOff uint64            // log offset just past the last finalized commit
	maxTxn       int               // max blocks in one transaction
	chunk        int               // blocks per write step
	lastFlush    time.Time

	// --- allocation state ---
	allocMu      sync.Mutex // guards counters below when readers call Statfs
	freeBlocks   uint64
	freeInodes   uint32
	bbFree       []uint32 // free bits per block-bitmap block
	ibFree       []uint32
	blockHint    uint32
	inodeHint    uint32
	dirtyBlocks  int
	reservedBlks uint64

	// --- in-memory per-inode state ---
	refMu sync.Mutex
	refs  map[uint32]uint64 // kernel lookup counts (inodes with nlink==0 live until forgotten)

	dirMu   sync.Mutex
	dirs    map[uint32]*dirIndex
	dirEnts int

	// --- snapshots (see snapshot.go) ---
	snapTable   *format.SnapTable
	snaps       [format.MaxSnapshots]*snapState
	snapMask    uint32 // active snapshots
	cowIdx      map[uint32][]*cowRec
	cowBlocks   []uint32
	cowFree     []*cowRec
	snapSys     map[uint32]struct{} // blocks that belong to the snapshot machinery itself
	snapViews   [format.MaxSnapshots]*FS
	snapReserve int
	snapSeq     uint32
	// set only on a snapshot view:
	snapParent *FS
	snapSlot   int

	stats  Stats
	closed bool
	stop   chan struct{}
	wg     sync.WaitGroup
}

func errno(e error) syscall.Errno {
	var en syscall.Errno
	if errors.As(e, &en) {
		return en
	}
	return syscall.EIO
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
