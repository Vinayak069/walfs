package engine

import (
	"fmt"
	"math/bits"
	"strings"
	"syscall"

	"github.com/vinayak-singh/walfs/internal/format"
)

// ---------------------------------------------------------------------------
// Snapshots: see internal/format/snap.go for the model. This file implements
// preservation (copy-on-write), transfer of freed blocks, creation, deletion,
// recovery at mount, and the read-only views.

type snapState struct {
	uid       uint32 // unique among snapshots seen by this mount (slots get reused)
	slot      int
	name      string
	created   int64
	state     uint32
	capList   uint32
	capBlocks []uint32
	bits      [][]byte // captured bitmap, one entry per bitmap block
}

func (s *snapState) captured(x uint32) bool {
	g := int(x / format.BitsPerBlock)
	return g < len(s.bits) && format.BitTest(s.bits[g], x%format.BitsPerBlock)
}

type cowRec struct {
	format.CowRec
	blk uint32 // COW log block holding the record
	idx int    // slot within that block
}

func (fs *FS) cowRecords() int {
	n := 0
	for _, rs := range fs.cowIdx {
		n += len(rs)
	}
	return n
}

// SnapInfo describes a snapshot.
type SnapInfo struct {
	Name      string
	Slot      int
	UID       uint32
	CreatedNS int64
}

// ListSnapshots returns the active snapshots ordered by slot.
func (fs *FS) ListSnapshots() []SnapInfo {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.listSnapshotsLocked()
}

func (fs *FS) listSnapshotsLocked() []SnapInfo {
	var out []SnapInfo
	for i := 0; i < format.MaxSnapshots; i++ {
		if s := fs.snaps[i]; s != nil && s.state == format.SnapActive {
			out = append(out, SnapInfo{Name: s.name, Slot: i, UID: s.uid, CreatedNS: s.created})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Preservation

// snapNeeds returns the set of active snapshots that still see block x in its
// current form and have no preserved copy of it yet.
func (fs *FS) snapNeeds(x uint32) uint32 {
	if fs.snapMask == 0 || !fs.sb.Preservable(uint64(x)) {
		return 0
	}
	if _, sys := fs.snapSys[x]; sys {
		return 0
	}
	var need uint32
	for m := fs.snapMask; m != 0; m &= m - 1 {
		s := bits.TrailingZeros32(m)
		if fs.snaps[s].captured(x) {
			need |= 1 << s
		}
	}
	if need == 0 {
		return 0
	}
	for _, r := range fs.cowIdx[x] {
		need &^= r.Mask
	}
	return need
}

// preserve copies old (the current committed contents of block x) to a fresh
// block if any snapshot still needs it. It must be called before x is
// modified.
func (fs *FS) preserve(x uint32, old []byte, meta bool) error {
	need := fs.snapNeeds(x)
	if need == 0 {
		return nil
	}
	p, err := fs.allocBlock(0, true)
	if err != nil {
		fs.aborted = err
		return syscall.EIO
	}
	pb := fs.c.getNew(uint64(p), false)
	copy(pb.data, old)
	fs.dirtyData(pb) // flushed before the commit that makes the new contents durable
	if err := fs.addCowRec(format.CowRec{X: x, P: p, Mask: need}); err != nil {
		return err
	}
	fs.stats.SnapPreserved++
	return nil
}

// preserveData is preserve for a clean, cached file-data (or directory-data)
// buffer that is about to be modified in place.
func (fs *FS) preserveData(b *buf) error {
	if fs.snapMask == 0 || b.isData || b.inTxn {
		return nil // already preserved (or not needed) earlier in this transaction
	}
	if fs.snapNeeds(uint32(b.blk)) == 0 {
		return nil
	}
	// Pin first (see metaBuf): the copy allocates, which may evict unpinned
	// buffers. Pinning does not change the contents.
	fs.dirtyData(b)
	return fs.preserve(uint32(b.blk), b.data, false)
}

// transferToSnapshots handles the freeing of a block that snapshots still see
// unchanged: instead of releasing it, ownership moves to the snapshots.
func (fs *FS) transferToSnapshots(blk uint32, class blockClass) (bool, error) {
	need := fs.snapNeeds(blk)
	if need == 0 {
		return false, nil
	}
	flags := uint32(format.CowTransferred)
	if class == classMeta {
		flags |= format.CowMeta
	}
	if err := fs.addCowRec(format.CowRec{X: blk, P: blk, Mask: need, Flags: flags}); err != nil {
		return false, err
	}
	fs.stats.SnapTransferred++
	return true, nil
}

// ---------------------------------------------------------------------------
// COW record log

func (fs *FS) addCowRec(r format.CowRec) error {
	if len(fs.cowFree) == 0 {
		if err := fs.growCowLog(); err != nil {
			return err
		}
	}
	slot := fs.cowFree[len(fs.cowFree)-1]
	fs.cowFree = fs.cowFree[:len(fs.cowFree)-1]
	if err := fs.writeCowSlot(slot, r); err != nil {
		fs.cowFree = append(fs.cowFree, slot)
		return err
	}
	slot.CowRec = r
	fs.cowIdx[r.X] = append(fs.cowIdx[r.X], slot)
	return nil
}

func (fs *FS) writeCowSlot(slot *cowRec, r format.CowRec) error {
	b, err := fs.metaBuf(uint64(slot.blk))
	if err != nil {
		return syscall.EIO
	}
	format.CowPut(b.data, slot.idx, r)
	return nil
}

// growCowLog appends a fresh block to the log.
func (fs *FS) growCowLog() error {
	nb, err := fs.allocBlock(0, true)
	if err != nil {
		fs.aborted = err
		return syscall.EIO
	}
	blk := uint32(nb)
	fs.snapSys[blk] = struct{}{}
	b := fs.newMeta(uint64(blk))
	format.EncodeCow(b.data, &format.CowBlock{})
	if n := len(fs.cowBlocks); n == 0 {
		fs.sb.CowHead = blk
		if err := fs.writeSuper(); err != nil {
			return err
		}
	} else {
		lb, err := fs.metaBuf(uint64(fs.cowBlocks[n-1]))
		if err != nil {
			return syscall.EIO
		}
		format.CowSetNext(lb.data, blk)
	}
	fs.cowBlocks = append(fs.cowBlocks, blk)
	for i := format.CowRecsPerBlock - 1; i >= 0; i-- {
		fs.cowFree = append(fs.cowFree, &cowRec{blk: blk, idx: i})
	}
	return nil
}

// ---------------------------------------------------------------------------
// Persistence helpers

func (fs *FS) writeSnapTable() error {
	b, err := fs.metaBuf(uint64(fs.sb.SnapTable))
	if err != nil {
		return syscall.EIO
	}
	copy(b.data, fs.snapTable.Marshal())
	return nil
}

func (fs *FS) syncTableEntry(s *snapState) {
	fs.snapTable.Entries[s.slot] = format.SnapEntry{
		State: s.state, CapList: s.capList, CapBlocks: uint32(len(s.capBlocks)),
		CreatedNS: s.created, Name: s.name,
	}
}

// ---------------------------------------------------------------------------
// Creation

func validSnapName(name string) error {
	if name == "" || name == "." || name == ".." {
		return syscall.EINVAL
	}
	if len(name) > format.SnapNameMax {
		return syscall.ENAMETOOLONG
	}
	if strings.ContainsAny(name, "/\x00") {
		return syscall.EINVAL
	}
	return nil
}

func (fs *FS) nextSnapUID() uint32 {
	fs.snapSeq++
	return fs.snapSeq
}

func (fs *FS) snapByName(name string) *snapState {
	for _, s := range fs.snaps {
		if s != nil && s.name == name {
			return s
		}
	}
	return nil
}

// CreateSnapshot takes a point-in-time snapshot of the whole filesystem. It
// costs a copy of the block bitmap, independent of how much data exists.
func (fs *FS) CreateSnapshot(name string) error {
	if err := validSnapName(name); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.snapParent != nil || fs.opts.ReadOnly || fs.aborted != nil {
		return syscall.EROFS
	}
	if fs.opts.Mode == ModeNone {
		return syscall.EOPNOTSUPP
	}
	if fs.snapByName(name) != nil {
		return syscall.EEXIST
	}
	slot := -1
	for i := 0; i < format.MaxSnapshots; i++ {
		if fs.snaps[i] == nil {
			slot = i
			break
		}
	}
	if slot < 0 {
		return syscall.ENOSPC
	}
	nbb := int(fs.sb.BlockBitmapBlocks)
	if nbb > format.MaxCapBlocks {
		return syscall.EFBIG
	}
	if nbb+8 > fs.maxTxn {
		return syscall.ENOSPC // journal too small to create a snapshot atomically
	}
	// The snapshot must see exactly the committed state, with nothing dirty
	// in the cache: commit first, then work in a fresh transaction.
	if err := fs.commit(); err != nil {
		return syscall.EIO
	}
	if err := fs.beginOp(nbb+8, nbb+4); err != nil {
		return err
	}
	if fs.freeBlocks < uint64(nbb+4)+uint64(fs.reservedBlks) {
		return syscall.ENOSPC
	}

	newSys := []uint32{}
	alloc := func() (uint32, error) {
		b, err := fs.allocBlock(0, true)
		if err != nil {
			return 0, err
		}
		newSys = append(newSys, b)
		return b, nil
	}
	created := false
	if fs.sb.SnapTable == 0 {
		tb, err := alloc()
		if err != nil {
			return errnoOrNil(err)
		}
		fs.snapTable = &format.SnapTable{}
		fs.sb.SnapTable = tb
		created = true
		fs.newMeta(uint64(tb))
	}
	listBlk, err := alloc()
	if err != nil {
		return errnoOrNil(err)
	}
	st := &snapState{uid: fs.nextSnapUID(), slot: slot, name: name, created: fs.now(), state: format.SnapActive, capList: listBlk}
	for i := 0; i < nbb; i++ {
		b, err := alloc()
		if err != nil {
			return errnoOrNil(err)
		}
		st.capBlocks = append(st.capBlocks, b)
	}
	// Copy the allocation bitmap as of now (including the blocks just taken
	// for the snapshot itself), then remove everything that is not part of
	// the filesystem image the snapshot should show.
	st.bits = make([][]byte, nbb)
	for g := 0; g < nbb; g++ {
		bm, err := fs.c.get(fs.sb.BlockBitmapStart + uint64(g))
		if err != nil {
			return syscall.EIO
		}
		st.bits[g] = append([]byte(nil), bm.data...)
	}
	clearBit := func(x uint32) {
		if g := int(x / format.BitsPerBlock); g < nbb {
			format.BitClear(st.bits[g], x%format.BitsPerBlock)
		}
	}
	for _, x := range newSys {
		clearBit(x)
	}
	for x := range fs.snapSys {
		clearBit(x)
	}
	for _, rs := range fs.cowIdx {
		for _, r := range rs {
			clearBit(r.P)
		}
	}
	for i, blk := range st.capBlocks {
		cb := fs.newMeta(uint64(blk))
		copy(cb.data, st.bits[i])
	}
	lb := fs.newMeta(uint64(listBlk))
	copy(lb.data, format.EncodeCapList(st.capBlocks))

	fs.snaps[slot] = st
	fs.syncTableEntry(st)
	if err := fs.writeSnapTable(); err != nil {
		return err
	}
	if created {
		if err := fs.writeSuper(); err != nil {
			return err
		}
	}
	for _, x := range newSys {
		fs.snapSys[x] = struct{}{}
	}
	fs.snapMask |= 1 << slot
	return nil
}

// ---------------------------------------------------------------------------
// Deletion

// DeleteSnapshot removes a snapshot and releases every block only it needed.
// Large snapshots are released over several transactions; a crash in between
// leaves the snapshot in the "deleting" state and mount finishes the job.
func (fs *FS) DeleteSnapshot(name string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.snapParent != nil || fs.opts.ReadOnly || fs.aborted != nil {
		return syscall.EROFS
	}
	s := fs.snapByName(name)
	if s == nil || s.state != format.SnapActive {
		return syscall.ENOENT
	}
	if err := fs.beginOp(reserveMeta, 0); err != nil {
		return err
	}
	s.state = format.SnapDeleting
	fs.snapMask &^= 1 << s.slot
	fs.syncTableEntry(s)
	if err := fs.writeSnapTable(); err != nil {
		return err
	}
	fs.snapViews[s.slot] = nil
	return fs.finishDelete(s)
}

const snapDeleteChunk = 192

func (fs *FS) removeFromIdx(x uint32, r *cowRec) {
	rs := fs.cowIdx[x]
	for i, c := range rs {
		if c == r {
			rs = append(rs[:i], rs[i+1:]...)
			break
		}
	}
	if len(rs) == 0 {
		delete(fs.cowIdx, x)
	} else {
		fs.cowIdx[x] = rs
	}
}

// recordsWith returns up to limit records whose mask contains bit.
func (fs *FS) recordsWith(bit uint32, limit int) []*cowRec {
	var out []*cowRec
	for _, rs := range fs.cowIdx {
		for _, r := range rs {
			if r.Mask&bit != 0 {
				out = append(out, r)
				if len(out) == limit {
					return out
				}
			}
		}
	}
	return out
}

// finishDelete clears the snapshot's bit from every record and releases
// records (and their blocks) that no snapshot needs any more. Progress lives
// in the masks themselves, so the walk can be interrupted at any point.
func (fs *FS) finishDelete(s *snapState) error {
	bit := uint32(1) << s.slot
	for {
		batch := fs.recordsWith(bit, snapDeleteChunk)
		for _, r := range batch {
			nr := r.CowRec
			nr.Mask &^= bit
			if nr.Mask != 0 {
				if err := fs.writeCowSlot(r, nr); err != nil {
					return err
				}
				r.CowRec = nr
				continue
			}
			// The last snapshot that needed this block: release it.
			if err := fs.writeCowSlot(r, format.CowRec{}); err != nil {
				return err
			}
			class := classData
			if r.Flags&format.CowMeta != 0 {
				class = classMeta
			}
			x, p := r.X, r.P
			fs.removeFromIdx(x, r)
			r.CowRec = format.CowRec{}
			fs.cowFree = append(fs.cowFree, r)
			if err := fs.freeBlockRaw(p, class); err != nil {
				return err
			}
		}
		if len(batch) < snapDeleteChunk {
			break
		}
		if err := fs.beginOp(reserveMeta+snapDeleteChunk/2, 0); err != nil {
			return err
		}
	}
	// All records are clean: release the snapshot's own structures.
	for _, b := range s.capBlocks {
		delete(fs.snapSys, b)
		if err := fs.freeBlockRaw(b, classMeta); err != nil {
			return err
		}
	}
	delete(fs.snapSys, s.capList)
	if err := fs.freeBlockRaw(s.capList, classMeta); err != nil {
		return err
	}
	fs.snapTable.Entries[s.slot] = format.SnapEntry{}
	fs.snaps[s.slot] = nil
	return fs.writeSnapTable()
}

// ---------------------------------------------------------------------------
// Mount-time loading

func (fs *FS) loadSnapshots() error {
	fs.cowIdx = map[uint32][]*cowRec{}
	fs.snapSys = map[uint32]struct{}{}
	fs.snapReserve = reserveMeta + 8
	if fs.sb.SnapTable == 0 {
		fs.snapTable = &format.SnapTable{}
		return nil
	}
	tb, err := fs.c.get(uint64(fs.sb.SnapTable))
	if err != nil {
		return err
	}
	if fs.snapTable, err = format.ParseSnapTable(tb.data); err != nil {
		return err
	}
	fs.snapSys[fs.sb.SnapTable] = struct{}{}
	for i, e := range fs.snapTable.Entries {
		if e.State == format.SnapFree {
			continue
		}
		s := &snapState{uid: fs.nextSnapUID(), slot: i, name: e.Name, created: e.CreatedNS, state: e.State, capList: e.CapList}
		lb, err := fs.c.get(uint64(e.CapList))
		if err != nil {
			return err
		}
		if s.capBlocks, err = format.ParseCapList(lb.data); err != nil {
			return fmt.Errorf("snapshot %q: %w", e.Name, err)
		}
		if len(s.capBlocks) != int(fs.sb.BlockBitmapBlocks) {
			return corruptf("snapshot %q: capture bitmap has %d blocks, expected %d", e.Name, len(s.capBlocks), fs.sb.BlockBitmapBlocks)
		}
		fs.snapSys[e.CapList] = struct{}{}
		for _, cb := range s.capBlocks {
			if uint64(cb) >= fs.sb.TotalBlocks {
				return corruptf("snapshot %q: capture block %d out of range", e.Name, cb)
			}
			b, err := fs.c.get(uint64(cb))
			if err != nil {
				return err
			}
			s.bits = append(s.bits, append([]byte(nil), b.data...))
			fs.snapSys[cb] = struct{}{}
		}
		fs.snaps[i] = s
		if s.state == format.SnapActive {
			fs.snapMask |= 1 << i
		}
	}
	// COW record log.
	for blk, n := fs.sb.CowHead, 0; blk != 0; n++ {
		if n > int(fs.sb.TotalBlocks) || uint64(blk) >= fs.sb.TotalBlocks {
			return corruptf("COW log chain is broken or cyclic")
		}
		b, err := fs.c.get(uint64(blk))
		if err != nil {
			return err
		}
		cb, err := format.ParseCow(b.data)
		if err != nil {
			return err
		}
		fs.snapSys[blk] = struct{}{}
		fs.cowBlocks = append(fs.cowBlocks, blk)
		for i := format.CowRecsPerBlock - 1; i >= 0; i-- {
			r := &cowRec{CowRec: cb.Recs[i], blk: blk, idx: i}
			if r.Empty() {
				fs.cowFree = append(fs.cowFree, r)
				continue
			}
			fs.cowIdx[r.X] = append(fs.cowIdx[r.X], r)
		}
		blk = cb.Next
	}
	return nil
}

// resumeDeletions finishes snapshot deletions interrupted by a crash.
func (fs *FS) resumeDeletions() error {
	for _, s := range fs.snaps {
		if s != nil && s.state == format.SnapDeleting {
			if err := fs.beginOp(reserveMeta, 0); err != nil {
				return err
			}
			if err := fs.finishDelete(s); err != nil {
				return err
			}
		}
	}
	return nil
}
