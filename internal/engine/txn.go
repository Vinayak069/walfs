package engine

import (
	"fmt"
	"slices"
	"syscall"
	"time"

	"github.com/vinayak-singh/walfs/internal/format"
)

// ---------------------------------------------------------------------------
// Joining blocks to the running transaction
//
// There is exactly one running transaction. Operations run under the FS write
// lock and never straddle a commit, so a transaction is a prefix of whole
// operations: after a crash the recovered state is always some operation
// boundary.

// txBlocks is the number of journal blocks (excluding descriptors/commit) the
// running transaction would occupy if committed now.
func (fs *FS) txBlocks() int {
	return len(fs.txMeta) + (len(fs.txRevoke)+format.DescCapacity-1)/format.DescCapacity
}

// joinTxn adds a buffer to the running transaction as a journaled block.
func (fs *FS) joinTxn(b *buf) {
	if b.inTxn {
		return
	}
	if b.ckpt && b.frozen == nil {
		// The last committed image of this block has not reached its home
		// location yet. Preserve it: the checkpoint must write committed
		// state only, never the half-finished running transaction.
		b.frozen = fs.c.alloc()
		copy(b.frozen, b.data)
	}
	b.inTxn = true
	fs.txMeta[b.blk] = b
}

// metaBuf returns blk for modification as journaled metadata.
func (fs *FS) metaBuf(blk uint64) (*buf, error) {
	b, err := fs.c.get(blk)
	if err != nil {
		return nil, err
	}
	if !b.inTxn && fs.snapMask != 0 {
		// The block is about to change for the first time in this
		// transaction: snapshots that still see its current contents get a
		// copy first. b.data is the last committed image here.
		//
		// Pin the buffer before preserving: making the copy allocates, and
		// allocation may evict unpinned cache entries, including this one.
		// joinTxn does not touch the contents, so b.data stays the old image.
		fs.joinTxn(b)
		if err := fs.preserve(uint32(blk), b.data, true); err != nil {
			return nil, err
		}
		return b, nil
	}
	fs.joinTxn(b)
	return b, nil
}

// newMeta returns a zeroed, journaled buffer for a freshly allocated metadata
// block (indirect block, directory block) without reading the device.
func (fs *FS) newMeta(blk uint64) *buf {
	b := fs.c.getNew(blk, true)
	fs.joinTxn(b)
	return b
}

// dirtyData registers a modified file-data buffer with the running transaction.
func (fs *FS) dirtyData(b *buf) {
	if fs.opts.Mode == ModeData {
		fs.joinTxn(b)
		return
	}
	if !b.isData {
		b.isData = true
		fs.txData = append(fs.txData, b)
	}
}

// beginOp must be called before every mutating operation (and between the
// chunks of a long one, once all in-flight state has been written back into
// buffers). It makes sure the operation's worst-case footprint fits in the
// running transaction and that enough blocks are actually allocatable.
func (fs *FS) beginOp(reserve, needBlocks int) error {
	if fs.aborted != nil || fs.opts.ReadOnly {
		return syscall.EROFS
	}
	full := fs.txBlocks()+reserve > fs.maxTxn || len(fs.txData) >= fs.opts.DirtyLimitBlocks
	if !full && len(fs.freedPending) > 0 {
		avail := int64(fs.freeBlocks) - int64(len(fs.freedPending))
		if avail < int64(needBlocks)+int64(fs.reservedBlks) {
			full = true // let freed blocks become reusable
		}
	}
	if full {
		if err := fs.commit(); err != nil {
			return syscall.EIO
		}
	}
	if fs.snapMask != 0 {
		// Overwriting snapshotted blocks needs spare blocks for the copies.
		// Check up front so that an operation never fails half-way.
		required := int64(needBlocks) + int64(reserve) + 8
		avail := func() int64 { return int64(fs.freeBlocks) - int64(len(fs.freedPending)) }
		if avail() < required {
			if err := fs.commit(); err != nil {
				return syscall.EIO
			}
			if avail() < required {
				return syscall.ENOSPC
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Commit

func (fs *FS) flush() error {
	fs.stats.Flushes++
	return fs.dev.Flush()
}

// commit makes the running transaction durable. Order of events (the
// write-ahead-log protocol):
//
//  1. ordered mode: write the transaction's new data blocks to their final
//     locations (they are unreachable until step 4, so this is safe);
//  2. write revoke, descriptor and journaled block images to the log;
//  3. barrier (everything above is durable);
//  4. write the commit block carrying a CRC over steps 1-2's log blocks;
//  5. barrier. Only now is the transaction committed.
//
// Home locations of journaled blocks are written later, by checkpoint().
func (fs *FS) commit() error {
	if fs.aborted != nil {
		return fs.aborted
	}
	if len(fs.txMeta) == 0 && len(fs.txData) == 0 && len(fs.txRevoke) == 0 {
		return nil
	}
	t0 := time.Now()
	var err error
	if fs.opts.Mode == ModeNone {
		err = fs.commitNoJournal()
	} else {
		err = fs.commitJournal()
	}
	if err != nil {
		fs.aborted = err
		return err
	}
	fs.lastFlush = time.Now()
	fs.stats.observeCommit(time.Since(t0))
	fs.stats.Commits++
	return nil
}

// sortedMeta returns the journaled buffers ordered by block number.
func (fs *FS) sortedMeta() []*buf {
	bs := make([]*buf, 0, len(fs.txMeta))
	for _, b := range fs.txMeta {
		bs = append(bs, b)
	}
	slices.SortFunc(bs, func(a, b *buf) int { return cmp64(a.blk, b.blk) })
	return bs
}

func cmp64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// homeWriter batches block writes to their home locations, merging runs of
// adjacent blocks into large sequential writes.
type homeWriter struct {
	fs      *FS
	stage   []byte
	start   uint64
	n       int
	written uint64
}

const stageBlocks = 256 // 1 MiB

func newHomeWriter(fs *FS) *homeWriter {
	return &homeWriter{fs: fs, stage: make([]byte, stageBlocks*format.BlockSize)}
}

func (w *homeWriter) put(blk uint64, data []byte) error {
	if w.n > 0 && (blk != w.start+uint64(w.n) || w.n == stageBlocks) {
		if err := w.flush(); err != nil {
			return err
		}
	}
	if w.n == 0 {
		w.start = blk
	}
	copy(w.stage[w.n*format.BlockSize:], data)
	w.n++
	return nil
}

func (w *homeWriter) flush() error {
	if w.n == 0 {
		return nil
	}
	_, err := w.fs.dev.WriteAt(w.stage[:w.n*format.BlockSize], int64(w.start)*format.BlockSize)
	w.written += uint64(w.n)
	w.n = 0
	return err
}

// writeOrderedData writes the transaction's data blocks home.
func (fs *FS) writeOrderedData() error {
	if len(fs.txData) == 0 {
		return nil
	}
	slices.SortFunc(fs.txData, func(a, b *buf) int { return cmp64(a.blk, b.blk) })
	w := newHomeWriter(fs)
	for _, b := range fs.txData {
		if err := w.put(b.blk, b.data); err != nil {
			return err
		}
	}
	if err := w.flush(); err != nil {
		return err
	}
	fs.stats.DataBlocks += w.written
	return nil
}

// releaseData drops committed ordered-data buffers (the kernel page cache
// holds file data; keeping a second copy here would only waste memory).
func (fs *FS) releaseData() {
	for _, b := range fs.txData {
		b.isData = false
		fs.c.forget(b)
	}
	fs.txData = fs.txData[:0]
}

func (fs *FS) commitNoJournal() error {
	if err := fs.writeOrderedData(); err != nil {
		return err
	}
	w := newHomeWriter(fs)
	metas := fs.sortedMeta()
	for _, b := range metas {
		if err := w.put(b.blk, b.data); err != nil {
			return err
		}
	}
	if err := w.flush(); err != nil {
		return err
	}
	if err := fs.flush(); err != nil {
		return err
	}
	for _, b := range metas {
		b.inTxn = false
	}
	fs.releaseData()
	clear(fs.txMeta)
	clear(fs.txRevoke)
	clear(fs.freedPending)
	return nil
}

// logWriter streams blocks into the circular log, splitting writes at the
// wrap point, and maintains the transaction's chained CRC.
type logWriter struct {
	fs    *FS
	off   uint64 // logical offset of the next block
	stage []byte
	start uint64 // logical offset of stage[0]
	n     int
	crc   uint32
	bytes uint64
}

func (fs *FS) newLogWriter(seq uint64) *logWriter {
	return &logWriter{
		fs: fs, off: fs.headOff, start: fs.headOff,
		stage: make([]byte, stageBlocks*format.BlockSize),
		crc:   format.TxnCRCSeed(fs.sb.UUID, seq),
	}
}

func (w *logWriter) emit(block []byte, inCRC bool) error {
	if inCRC {
		w.crc = format.CRC32C(w.crc, block)
	}
	copy(w.stage[w.n*format.BlockSize:], block)
	w.n++
	w.off++
	capacity := w.fs.jsb.Capacity()
	if w.n == stageBlocks || w.off%capacity == 0 {
		return w.flushStage()
	}
	return nil
}

func (w *logWriter) flushStage() error {
	if w.n == 0 {
		return nil
	}
	phys := w.fs.sb.JournalStart + 1 + w.start%w.fs.jsb.Capacity()
	_, err := w.fs.dev.WriteAt(w.stage[:w.n*format.BlockSize], int64(phys)*format.BlockSize)
	w.fs.stats.JournalBlocks += uint64(w.n)
	w.start += uint64(w.n)
	w.n = 0
	return err
}

func (fs *FS) journalUsed() uint64 { return fs.headOff - fs.jsb.TailOff }

func (fs *FS) commitJournal() error {
	if err := fs.writeOrderedData(); err != nil {
		return err
	}
	metas := fs.sortedMeta()
	revokes := make([]uint64, 0, len(fs.txRevoke))
	for blk := range fs.txRevoke {
		revokes = append(revokes, blk)
	}
	slices.Sort(revokes)

	nRevBlocks := (len(revokes) + format.DescCapacity - 1) / format.DescCapacity
	nDescBlocks := (len(metas) + format.DescCapacity - 1) / format.DescCapacity
	need := uint64(nRevBlocks + nDescBlocks + len(metas) + 1)
	if need > fs.jsb.Capacity() {
		return fmt.Errorf("transaction of %d blocks exceeds journal capacity %d", need, fs.jsb.Capacity())
	}
	if fs.journalUsed()+need > fs.jsb.Capacity() {
		if err := fs.checkpoint(); err != nil {
			return err
		}
	}

	seq := fs.nextSeq
	w := fs.newLogWriter(seq)
	for i := 0; i < len(revokes); i += format.DescCapacity {
		j := min(i+format.DescCapacity, len(revokes))
		if err := w.emit(format.EncodeRevoke(seq, revokes[i:j]), true); err != nil {
			return err
		}
	}
	for i := 0; i < len(metas); i += format.DescCapacity {
		j := min(i+format.DescCapacity, len(metas))
		targets := make([]uint64, 0, j-i)
		for _, b := range metas[i:j] {
			targets = append(targets, b.blk)
		}
		if err := w.emit(format.EncodeDescriptor(seq, targets), true); err != nil {
			return err
		}
		for _, b := range metas[i:j] {
			if err := w.emit(b.data, true); err != nil {
				return err
			}
		}
	}
	if err := w.flushStage(); err != nil {
		return err
	}
	// Barrier 1: data (ordered mode) and log blocks are durable before the
	// commit record can possibly be.
	if err := fs.flush(); err != nil {
		return err
	}
	nBefore := int(w.off - fs.headOff)
	commit := format.EncodeCommit(seq, nBefore, w.crc, fs.opts.Clock())
	if err := w.emit(commit, false); err != nil {
		return err
	}
	if err := w.flushStage(); err != nil {
		return err
	}
	// Barrier 2: the transaction is committed once this returns.
	if err := fs.flush(); err != nil {
		return err
	}

	// Post-commit bookkeeping: committed journaled blocks now wait for their
	// home-location write.
	for _, b := range metas {
		b.inTxn = false
		b.ckpt = true
		if b.frozen != nil {
			fs.c.release(b.frozen)
			b.frozen = nil
		}
		fs.ckptSet[b.blk] = b
	}
	fs.stats.Revoked += uint64(len(revokes))
	fs.releaseData()
	clear(fs.txMeta)
	clear(fs.txRevoke)
	clear(fs.freedPending)
	fs.headOff = w.off
	fs.nextSeq++

	if fs.journalUsed() > fs.jsb.Capacity()/2 {
		return fs.checkpoint()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Checkpoint

// checkpoint writes the committed image of every journaled block to its home
// location, makes that durable, and only then advances the journal tail,
// freeing log space. The ordering is the whole point: a log record may be
// discarded only after its contents live in the home location.
func (fs *FS) checkpoint() error {
	if fs.opts.Mode == ModeNone {
		return nil
	}
	if len(fs.ckptSet) == 0 && fs.jsb.TailOff == fs.headOff {
		return nil
	}
	bs := make([]*buf, 0, len(fs.ckptSet))
	for _, b := range fs.ckptSet {
		bs = append(bs, b)
	}
	slices.SortFunc(bs, func(a, b *buf) int { return cmp64(a.blk, b.blk) })
	w := newHomeWriter(fs)
	for _, b := range bs {
		img := b.data
		if b.frozen != nil {
			img = b.frozen
		}
		if err := w.put(b.blk, img); err != nil {
			fs.aborted = err
			return err
		}
	}
	if err := w.flush(); err != nil {
		fs.aborted = err
		return err
	}
	fs.stats.HomeMetaBlocks += w.written
	if err := fs.flush(); err != nil {
		fs.aborted = err
		return err
	}
	// Advance the tail; it must be durable before the space is reused.
	fs.jsb.TailOff, fs.jsb.TailSeq = fs.headOff, fs.nextSeq
	if _, err := fs.dev.WriteAt(fs.jsb.Marshal(), int64(fs.sb.JournalStart)*format.BlockSize); err != nil {
		fs.aborted = err
		return err
	}
	if err := fs.flush(); err != nil {
		fs.aborted = err
		return err
	}
	for _, b := range bs {
		b.ckpt = false
		if b.frozen != nil {
			fs.c.release(b.frozen)
			b.frozen = nil
		}
	}
	clear(fs.ckptSet)
	fs.stats.Checkpoints++
	return nil
}

// Sync commits the running transaction (fsync/sync semantics).
func (fs *FS) Sync() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.commit(); err != nil {
		return syscall.EIO
	}
	return nil
}

// Checkpoint forces a commit followed by a full checkpoint (empties the log).
func (fs *FS) Checkpoint() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.commit(); err != nil {
		return syscall.EIO
	}
	if err := fs.checkpoint(); err != nil {
		return syscall.EIO
	}
	return nil
}

// Stats returns a snapshot of the counters.
func (fs *FS) Stats() Stats {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.stats
}
