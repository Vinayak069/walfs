package engine

import (
	"errors"
	"fmt"
	"math/bits"
	"syscall"
	"time"

	"github.com/vinayak-singh/walfs/internal/blockdev"
	"github.com/vinayak-singh/walfs/internal/format"
)

// RecoveryInfo describes what mount-time recovery did.
type RecoveryInfo struct {
	Journal      *format.RecoverResult
	OrphansFreed int
}

// Open mounts the filesystem on dev: it replays any committed journal
// transactions, rebuilds the in-memory allocation counters, finishes freeing
// inodes that were unlinked while still open when the system went down, and
// starts the background commit timer.
func Open(dev blockdev.Device, opts Options) (*FS, *RecoveryInfo, error) {
	opts.fill()
	blk0 := make([]byte, format.BlockSize)
	if _, err := dev.ReadAt(blk0, 0); err != nil {
		return nil, nil, fmt.Errorf("reading superblock: %w", err)
	}
	sb, err := format.UnmarshalSuperblock(blk0)
	if err != nil {
		return nil, nil, err
	}
	if dev.Size() < int64(sb.TotalBlocks)*format.BlockSize {
		return nil, nil, fmt.Errorf("device (%d bytes) smaller than filesystem (%d blocks)", dev.Size(), sb.TotalBlocks)
	}
	j, err := format.ReadJournal(dev, sb)
	if err != nil {
		return nil, nil, err
	}
	info := &RecoveryInfo{}
	if info.Journal, err = j.Recover(); err != nil {
		return nil, nil, fmt.Errorf("journal recovery: %w", err)
	}
	// Replay may have rewritten block 0.
	if _, err := dev.ReadAt(blk0, 0); err != nil {
		return nil, nil, err
	}
	if sb, err = format.UnmarshalSuperblock(blk0); err != nil {
		return nil, nil, err
	}

	fs := &FS{
		dev: dev, sb: sb, opts: opts,
		c:            newCache(dev, opts.CacheBlocks),
		txMeta:       map[uint64]*buf{},
		txRevoke:     map[uint64]struct{}{},
		freedPending: map[uint64]struct{}{},
		ckptSet:      map[uint64]*buf{},
		refs:         map[uint32]uint64{},
		dirs:         map[uint32]*dirIndex{},
		jsb:          j.Super,
		headOff:      j.Super.TailOff,
		nextSeq:      j.Super.TailSeq,
		lastFlush:    time.Now(),
		stop:         make(chan struct{}),
	}
	capacity := int(fs.jsb.Capacity())
	fs.maxTxn = min(capacity/2, 16384)
	fs.chunk = max(4, min(64, fs.maxTxn/4))
	fs.reservedBlks = uint64(max(8, min(64, int(sb.TotalBlocks)/100)))
	if err := fs.loadCounters(); err != nil {
		return nil, nil, err
	}
	if !opts.ReadOnly {
		n, err := fs.recoverOrphans()
		if err != nil {
			return nil, nil, fmt.Errorf("orphan recovery: %w", err)
		}
		info.OrphansFreed = n
		if err := fs.commit(); err != nil {
			return nil, nil, err
		}
	}
	if opts.CommitInterval > 0 {
		fs.wg.Add(1)
		go fs.commitLoop()
	}
	return fs, info, nil
}

// loadCounters scans the bitmaps once to populate the free-space counters.
func (fs *FS) loadCounters() error {
	sb := fs.sb
	fs.bbFree = make([]uint32, sb.BlockBitmapBlocks)
	fs.ibFree = make([]uint32, sb.InodeBitmapBlocks)
	buf := make([]byte, format.BlockSize)
	count := func(start uint64, n uint64, total uint64, out []uint32) (free uint64, err error) {
		for g := uint64(0); g < n; g++ {
			if _, err := fs.dev.ReadAt(buf, int64(start+g)*format.BlockSize); err != nil {
				return 0, err
			}
			nbits := min(total-g*format.BitsPerBlock, format.BitsPerBlock)
			ones := uint64(0)
			full := nbits / 8
			for _, c := range buf[:full] {
				ones += uint64(bits.OnesCount8(c))
			}
			if rem := nbits % 8; rem != 0 {
				ones += uint64(bits.OnesCount8(buf[full] & (1<<rem - 1)))
			}
			out[g] = uint32(nbits - ones)
			free += nbits - ones
		}
		return free, nil
	}
	var err error
	if fs.freeBlocks, err = count(sb.BlockBitmapStart, sb.BlockBitmapBlocks, sb.TotalBlocks, fs.bbFree); err != nil {
		return err
	}
	fi, err := count(sb.InodeBitmapStart, sb.InodeBitmapBlocks, uint64(sb.InodeCount), fs.ibFree)
	fs.freeInodes = uint32(fi)
	return err
}

// recoverOrphans frees inodes that were unlinked while still referenced by
// open files at the time of a crash or unclean shutdown.
func (fs *FS) recoverOrphans() (int, error) {
	n := 0
	for fs.sb.OrphanHead != 0 {
		if n > int(fs.sb.InodeCount) {
			return n, corruptf("orphan list has a cycle")
		}
		ino := fs.sb.OrphanHead
		in, err := fs.getInode(ino)
		if err != nil {
			return n, err
		}
		if in.Mode == 0 || in.Nlink != 0 {
			return n, corruptf("orphan list entry %d is live (mode %o nlink %d)", ino, in.Mode, in.Nlink)
		}
		if err := fs.destroyInode(ino, &in, true); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (fs *FS) commitLoop() {
	defer fs.wg.Done()
	iv := fs.opts.CommitInterval
	t := time.NewTicker(iv / 2)
	defer t.Stop()
	for {
		select {
		case <-fs.stop:
			return
		case <-t.C:
			fs.mu.Lock()
			if fs.aborted == nil && time.Since(fs.lastFlush) >= iv {
				_ = fs.commit() // errors latch fs.aborted
				fs.lastFlush = time.Now()
			}
			fs.mu.Unlock()
		}
	}
}

// Close commits, checkpoints (leaving an empty journal) and stops background
// work. The device is not closed.
func (fs *FS) Close() error {
	if fs.opts.CommitInterval > 0 {
		close(fs.stop)
		fs.wg.Wait()
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.closed {
		return nil
	}
	fs.closed = true
	if fs.aborted != nil {
		return fs.aborted
	}
	if fs.opts.ReadOnly {
		return nil
	}
	if err := fs.commit(); err != nil {
		return err
	}
	return fs.checkpoint()
}

// Aborted returns the error that forced the filesystem read-only, if any.
func (fs *FS) Aborted() error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.aborted
}

// Superblock returns a copy of the geometry.
func (fs *FS) Superblock() format.Superblock { return *fs.sb }

// IsCorrupt reports whether err denotes on-disk corruption.
func IsCorrupt(err error) bool {
	var ce *corruptError
	return errors.As(err, &ce) || errors.Is(err, syscall.EUCLEAN)
}
