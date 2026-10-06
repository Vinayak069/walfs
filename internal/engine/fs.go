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
	Clock            func() int64  // nanosecond clock, injectable for tests
	ReadOnly         bool
	SkipRecovery     bool // fsck/diagnostics: do not replay the journal
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
	Commits        uint64
	Checkpoints    uint64
	Flushes        uint64
	JournalBlocks  uint64 // blocks written to the log (descriptors, data, commits)
	HomeMetaBlocks uint64 // metadata blocks written at checkpoint
	DataBlocks     uint64 // ordered data blocks written at commit
	Revoked        uint64
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
	jsb       format.JournalSuper
	headOff   uint64 // logical offset where the next transaction starts
	nextSeq   uint64
	ckptSet   map[uint64]*buf // committed buffers awaiting a home-location write
	maxTxn    int             // max blocks in one transaction
	chunk     int             // blocks per write step
	lastFlush time.Time

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
