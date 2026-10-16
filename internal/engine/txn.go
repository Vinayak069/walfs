package engine

import (
	"fmt"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/Vinayak069/walfs/internal/format"
)

// ---------------------------------------------------------------------------
// Transactions
//
// There is one *running* transaction that operations join, and at most one
// *committing* transaction whose disk I/O is in flight. Operations run under
// the FS write lock and never straddle a freeze, so a transaction is always a
// prefix of whole operations: after a crash the recovered state is some
// operation boundary.
//
// Committing is pipelined (as in jbd2):
//
//	freeze (lock held)   copy the running transaction's block images, give it a
//	                     sequence number and log space, start a new running one
//	I/O    (lock free)   write ordered data, log blocks, barrier, commit block,
//	                     barrier -- other operations proceed meanwhile
//	finalize (lock held) publish the images for checkpointing, release the
//	                     transaction's freed blocks
//
// Because the I/O works on private copies, writers never wait for the disk
// except when they would overrun the log or the dirty-data limit.

// txBlocks is the number of journal blocks (excluding descriptors/commit) the
// running transaction would occupy if committed now.
func (fs *FS) txBlocks() int {
	return len(fs.txMeta) + (len(fs.txRevoke)+format.DescCapacity-1)/format.DescCapacity
}

func (fs *FS) runningEmpty() bool {
	return len(fs.txMeta) == 0 && len(fs.txData) == 0 && len(fs.txRevoke) == 0
}

// joinTxn adds a buffer to the running transaction as a journaled block.
func (fs *FS) joinTxn(b *buf) {
	if b.inTxn {
		return
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

// pendingFree reports whether blk was freed by a transaction that has not been
// committed yet and therefore must not be reallocated.
func (fs *FS) pendingFree(blk uint64) bool {
	if _, ok := fs.freedPending[blk]; ok {
		return true
	}
	if t := fs.committing; t != nil {
		_, ok := t.freed[blk]
		return ok
	}
	return false
}

func (fs *FS) pendingFreeCount() int {
	n := len(fs.freedPending)
	if t := fs.committing; t != nil {
		n += len(t.freed)
	}
	return n
}

// beginOp must be called before every mutating operation (and between the
// chunks of a long one, once all in-flight state has been written back into
// buffers). It makes sure the operation's worst-case footprint fits in the
// running transaction and that enough blocks are actually allocatable.
func (fs *FS) beginOp(reserve, needBlocks int) error {
	if fs.aborted != nil || fs.opts.ReadOnly {
		return syscall.EROFS
	}
	hard := fs.txBlocks()+reserve > fs.maxTxn || len(fs.txData) >= fs.opts.DirtyLimitBlocks
	switch {
	case hard:
		// Make room: wait for the previous commit (if any) and start a new
		// one; the running transaction is then empty.
		if err := fs.rotate(); err != nil {
			return syscall.EIO
		}
	case fs.opts.AsyncCommit &&
		(fs.txBlocks()+reserve > fs.maxTxn/4 || len(fs.txData) >= fs.opts.DirtyLimitBlocks/4):
		// Start committing early so that, with luck, the log or dirty limit
		// is never hit while a commit is still in flight.
		fs.commitAsync()
	}
	if p := fs.pendingFreeCount(); p > 0 {
		if int64(fs.freeBlocks)-int64(p) < int64(needBlocks)+int64(fs.reservedBlks) {
			// Freed blocks only become reusable once their transaction is
			// durable: make that happen now.
			if err := fs.commit(); err != nil {
				return syscall.EIO
			}
		}
	}
	if fs.snapMask != 0 {
		// Overwriting snapshotted blocks needs spare blocks for the copies.
		// Check up front so that an operation never fails half-way.
		required := int64(needBlocks) + int64(reserve) + 8
		avail := func() int64 { return int64(fs.freeBlocks) - int64(fs.pendingFreeCount()) }
		if avail() < required {
			if err := fs.commit(); err != nil {
				return syscall.EIO
			}
			if avail() < required {
				return syscall.ENOSPC
			}
		}
	}
	return fs.aborted
}

// ---------------------------------------------------------------------------
// Commit

func (fs *FS) flush() error {
	fs.stats.Flushes++
	return fs.dev.Flush()
}

type blockImg struct {
	blk uint64
	img []byte
}

// commitTx is a frozen transaction: private copies of everything it will write.
type commitTx struct {
	seq        uint64
	start, end uint64 // logical log offsets [start, end)
	meta       []blockImg
	data       []blockImg
	dataBufs   []*buf // cache buffers whose contents were copied into data
	revokes    []uint64
	freed      map[uint64]struct{}
	t0         time.Time

	err       error
	ioDone    chan struct{} // closed when the I/O has finished (successfully or not)
	done      chan struct{} // closed after finalize
	finalized bool

	journalBlocks, dataBlocks, flushes uint64
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

// freezeLocked turns the running transaction into a commitTx and starts a new
// running transaction. The caller holds the lock and has made sure that no
// commit is in flight.
func (fs *FS) freezeLocked() (*commitTx, error) {
	metas := fs.sortedMeta()
	revokes := make([]uint64, 0, len(fs.txRevoke))
	for blk := range fs.txRevoke {
		revokes = append(revokes, blk)
	}
	slices.Sort(revokes)

	nRev := (len(revokes) + format.DescCapacity - 1) / format.DescCapacity
	nDesc := (len(metas) + format.DescCapacity - 1) / format.DescCapacity
	need := uint64(nRev + nDesc + len(metas) + 1)
	capacity := fs.jsb.Capacity()
	if need > capacity {
		return nil, fmt.Errorf("transaction of %d blocks exceeds journal capacity %d", need, capacity)
	}
	if fs.journalUsed()+need > capacity {
		// The log is full: everything committed must reach its home
		// location before the space can be reused.
		if err := fs.checkpoint(); err != nil {
			return nil, err
		}
	}

	tx := &commitTx{
		seq: fs.nextSeq, start: fs.headOff, end: fs.headOff + need,
		revokes: revokes, freed: fs.freedPending, t0: time.Now(),
		ioDone: make(chan struct{}), done: make(chan struct{}),
	}
	fs.nextSeq++
	fs.headOff = tx.end

	tx.meta = make([]blockImg, len(metas))
	for i, b := range metas {
		img := fs.c.alloc()
		copy(img, b.data)
		tx.meta[i] = blockImg{b.blk, img}
		b.inTxn = false
		b.ckpt = true // pinned until its committed image reaches the home location
	}
	slices.SortFunc(fs.txData, func(a, b *buf) int { return cmp64(a.blk, b.blk) })
	tx.data = make([]blockImg, len(fs.txData))
	tx.dataBufs = fs.txData
	for i, b := range fs.txData {
		img := fs.c.alloc()
		copy(img, b.data)
		tx.data[i] = blockImg{b.blk, img}
		b.isData = false
		b.flushing = true // stays cached (and visible to readers) until written
	}
	fs.txData = nil
	clear(fs.txMeta)
	clear(fs.txRevoke)
	fs.freedPending = map[uint64]struct{}{}
	return tx, nil
}

// logWriter streams blocks into the circular log, splitting writes at the
// wrap point, and maintains the transaction's chained CRC.
type logWriter struct {
	fs      *FS
	off     uint64 // logical offset of the next block
	stage   []byte
	start   uint64 // logical offset of stage[0]
	n       int
	crc     uint32
	written uint64
	pooled  *[]byte
}

func (fs *FS) newLogWriter(start, seq uint64) *logWriter {
	sp := stagePool.Get().(*[]byte)
	return &logWriter{
		fs: fs, off: start, start: start,
		stage: *sp, pooled: sp,
		crc: format.TxnCRCSeed(fs.sb.UUID, seq),
	}
}

func (w *logWriter) release() {
	if w.pooled != nil {
		stagePool.Put(w.pooled)
		w.pooled, w.stage = nil, nil
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
	w.written += uint64(w.n)
	w.start += uint64(w.n)
	w.n = 0
	return err
}

// writeCommit performs the I/O of a frozen transaction. It touches only the
// transaction's private copies and immutable filesystem fields, so it runs
// without the lock. Order of events (the write-ahead-log protocol):
//
//  1. ordered mode: write the transaction's data blocks to their final
//     locations (they are unreachable until step 4, so this is safe);
//  2. write revoke, descriptor and journaled block images to the log;
//  3. barrier (everything above is durable);
//  4. write the commit block carrying a CRC over steps 1-2's log blocks;
//  5. barrier. Only now is the transaction committed.
//
// Home locations of journaled blocks are written later, by checkpoint().
func (fs *FS) writeCommit(tx *commitTx) error {
	if len(tx.data) > 0 {
		w := newHomeWriter(fs)
		defer w.release()
		for _, d := range tx.data {
			if err := w.put(d.blk, d.img); err != nil {
				return err
			}
		}
		if err := w.flush(); err != nil {
			return err
		}
		tx.dataBlocks = w.written
	}

	w := fs.newLogWriter(tx.start, tx.seq)
	defer w.release()
	for i := 0; i < len(tx.revokes); i += format.DescCapacity {
		j := min(i+format.DescCapacity, len(tx.revokes))
		if err := w.emit(format.EncodeRevoke(tx.seq, tx.revokes[i:j]), true); err != nil {
			return err
		}
	}
	for i := 0; i < len(tx.meta); i += format.DescCapacity {
		j := min(i+format.DescCapacity, len(tx.meta))
		targets := make([]uint64, 0, j-i)
		for _, m := range tx.meta[i:j] {
			targets = append(targets, m.blk)
		}
		if err := w.emit(format.EncodeDescriptor(tx.seq, targets), true); err != nil {
			return err
		}
		for _, m := range tx.meta[i:j] {
			if err := w.emit(m.img, true); err != nil {
				return err
			}
		}
	}
	if err := w.flushStage(); err != nil {
		return err
	}
	// Barrier 1: data (ordered mode) and log blocks are durable before the
	// commit record can possibly be.
	if err := fs.dev.Flush(); err != nil {
		return err
	}
	tx.flushes++
	commit := format.EncodeCommit(tx.seq, int(w.off-tx.start), w.crc, fs.opts.Clock())
	if err := w.emit(commit, false); err != nil {
		return err
	}
	if err := w.flushStage(); err != nil {
		return err
	}
	// Barrier 2: the transaction is committed once this returns.
	if err := fs.dev.Flush(); err != nil {
		return err
	}
	tx.flushes++
	tx.journalBlocks = w.written
	return nil
}

// finalizeLocked publishes a transaction whose I/O has finished. Idempotent.
func (fs *FS) finalizeLocked(tx *commitTx) {
	if tx.finalized {
		return
	}
	tx.finalized = true
	if tx.err != nil {
		if fs.aborted == nil {
			fs.aborted = tx.err
		}
	} else {
		for _, m := range tx.meta {
			if old, ok := fs.ckptImg[m.blk]; ok {
				fs.c.release(old)
			}
			fs.ckptImg[m.blk] = m.img
		}
		// Blocks freed by this (now durable) transaction no longer need their
		// committed images written home: the revoke record keeps recovery from
		// replaying them, and they may be reused from this point on.
		for blk := range tx.freed {
			if img, ok := fs.ckptImg[blk]; ok {
				fs.c.release(img)
				delete(fs.ckptImg, blk)
			}
		}
		for _, d := range tx.data {
			fs.c.release(d.img)
		}
		for _, b := range tx.dataBufs {
			b.flushing = false
			if !b.pinned() {
				fs.c.forget(b)
			}
		}
		fs.committedOff = tx.end
		fs.stats.JournalBlocks += tx.journalBlocks
		fs.stats.DataBlocks += tx.dataBlocks
		fs.stats.Flushes += tx.flushes
		fs.stats.Revoked += uint64(len(tx.revokes))
		fs.stats.Commits++
		fs.stats.observeCommit(time.Since(tx.t0))
		fs.lastFlush = time.Now()
	}
	if fs.committing == tx {
		fs.committing = nil
	}
	close(tx.done)
}

// drain waits for the in-flight commit (if any) and finalizes it. The lock
// stays held: the I/O needs nothing from us, so this cannot deadlock.
func (fs *FS) drain() error {
	if t := fs.committing; t != nil {
		<-t.ioDone
		fs.finalizeLocked(t)
	}
	return fs.aborted
}

func (fs *FS) runCommit(tx *commitTx) {
	tx.err = fs.writeCommit(tx)
	close(tx.ioDone)
	fs.mu.Lock()
	fs.finalizeLocked(tx)
	if tx.err == nil && fs.aborted == nil && !fs.closed && fs.committedOff-fs.jsb.TailOff > fs.jsb.Capacity()/2 {
		_ = fs.checkpoint() // errors latch fs.aborted
	}
	fs.mu.Unlock()
}

// commitAsync starts committing the running transaction in the background if
// no commit is in flight. It never blocks on I/O.
func (fs *FS) commitAsync() {
	if fs.aborted != nil || fs.committing != nil || fs.runningEmpty() {
		return
	}
	if fs.opts.Mode == ModeNone || !fs.opts.AsyncCommit {
		_ = fs.commit()
		return
	}
	tx, err := fs.freezeLocked()
	if err != nil {
		fs.aborted = err
		return
	}
	fs.committing = tx
	go fs.runCommit(tx)
}

// rotate makes room in the running transaction: it waits for the previous
// commit and starts a new one, after which the running transaction is empty.
func (fs *FS) rotate() error {
	if fs.opts.Mode == ModeNone || !fs.opts.AsyncCommit {
		return fs.commit()
	}
	if err := fs.drain(); err != nil {
		return err
	}
	fs.commitAsync()
	return fs.aborted
}

// commit makes everything issued so far durable and returns when it is. It
// holds the lock throughout, exactly like a synchronous commit.
func (fs *FS) commit() error {
	if fs.aborted != nil {
		return fs.aborted
	}
	if fs.opts.Mode == ModeNone {
		if fs.runningEmpty() {
			return nil
		}
		t0 := time.Now()
		if err := fs.commitNoJournal(); err != nil {
			fs.aborted = err
			return err
		}
		fs.lastFlush = time.Now()
		fs.stats.observeCommit(time.Since(t0))
		fs.stats.Commits++
		return nil
	}
	if err := fs.drain(); err != nil {
		return err
	}
	if fs.runningEmpty() {
		return nil
	}
	tx, err := fs.freezeLocked()
	if err != nil {
		fs.aborted = err
		return err
	}
	fs.committing = tx
	tx.err = fs.writeCommit(tx)
	close(tx.ioDone)
	fs.finalizeLocked(tx)
	if fs.aborted != nil {
		return fs.aborted
	}
	if fs.committedOff-fs.jsb.TailOff > fs.jsb.Capacity()/2 {
		return fs.checkpoint()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Journal-less mode (benchmark baseline and negative control)

// homeWriter batches block writes to their home locations, merging runs of
// adjacent blocks into large sequential writes.
type homeWriter struct {
	fs      *FS
	stage   []byte
	start   uint64
	n       int
	written uint64
	pooled  *[]byte
}

const stageBlocks = 256 // 1 MiB

// stagePool recycles the 1 MiB staging buffers: allocating (and zeroing, and
// later garbage-collecting) one per commit dominated the cost of small
// commits such as fsync of a single 4 KiB write.
var stagePool = sync.Pool{New: func() any { b := make([]byte, stageBlocks*format.BlockSize); return &b }}

func newHomeWriter(fs *FS) *homeWriter {
	sp := stagePool.Get().(*[]byte)
	return &homeWriter{fs: fs, stage: *sp, pooled: sp}
}

func (w *homeWriter) release() {
	if w.pooled != nil {
		stagePool.Put(w.pooled)
		w.pooled, w.stage = nil, nil
	}
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

func (fs *FS) commitNoJournal() error {
	slices.SortFunc(fs.txData, func(a, b *buf) int { return cmp64(a.blk, b.blk) })
	w := newHomeWriter(fs)
	defer w.release()
	for _, b := range fs.txData {
		if err := w.put(b.blk, b.data); err != nil {
			return err
		}
	}
	if err := w.flush(); err != nil {
		return err
	}
	fs.stats.DataBlocks += w.written
	w.written = 0
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
	for _, b := range fs.txData {
		b.isData = false
		fs.c.forget(b)
	}
	fs.txData = fs.txData[:0]
	clear(fs.txMeta)
	clear(fs.txRevoke)
	clear(fs.freedPending)
	return nil
}

// ---------------------------------------------------------------------------
// Checkpoint

func (fs *FS) journalUsed() uint64 { return fs.headOff - fs.jsb.TailOff }

// checkpoint writes the committed image of every journaled block to its home
// location, makes that durable, and only then advances the journal tail,
// freeing log space. The ordering is the whole point: a log record may be
// discarded only after its contents live in the home location.
func (fs *FS) checkpoint() error {
	if fs.opts.Mode == ModeNone {
		return nil
	}
	if err := fs.drain(); err != nil {
		return err
	}
	if len(fs.ckptImg) == 0 && fs.jsb.TailOff == fs.headOff {
		return nil
	}
	blks := make([]uint64, 0, len(fs.ckptImg))
	for blk := range fs.ckptImg {
		blks = append(blks, blk)
	}
	slices.Sort(blks)
	w := newHomeWriter(fs)
	defer w.release()
	for _, blk := range blks {
		if err := w.put(blk, fs.ckptImg[blk]); err != nil {
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
	for blk, img := range fs.ckptImg {
		fs.c.release(img)
		if b := fs.c.peek(blk); b != nil {
			b.ckpt = false
		}
	}
	clear(fs.ckptImg)
	fs.stats.Checkpoints++
	return nil
}

// Sync makes every operation issued so far durable (fsync/sync semantics).
// While its commit is in flight, other operations continue; fsyncs arriving
// meanwhile share the next commit (group commit).
func (fs *FS) Sync() error {
	fs.mu.Lock()
	if !fs.opts.AsyncCommit || fs.opts.Mode == ModeNone {
		err := fs.commit()
		fs.mu.Unlock()
		if err != nil {
			return syscall.EIO
		}
		return nil
	}
	for {
		if fs.aborted != nil {
			fs.mu.Unlock()
			return syscall.EIO
		}
		if t := fs.committing; t != nil && !fs.runningEmpty() {
			// An older commit is in flight and our operations sit in the
			// running transaction: wait for the older one, then commit ours.
			fs.mu.Unlock()
			<-t.done
			fs.mu.Lock()
			continue
		}
		if fs.committing == nil {
			if fs.runningEmpty() {
				fs.mu.Unlock()
				return nil
			}
			fs.commitAsync()
			if fs.committing == nil {
				continue // aborted
			}
		}
		t := fs.committing
		fs.mu.Unlock()
		<-t.done
		fs.mu.Lock()
		err := t.err
		fs.mu.Unlock()
		if err != nil {
			return syscall.EIO
		}
		return nil
	}
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
