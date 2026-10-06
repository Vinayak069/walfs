package engine

import (
	"encoding/binary"
	"math/bits"
	"syscall"

	"github.com/vinayak-singh/walfs/internal/format"
)

// ---------------------------------------------------------------------------
// Block and inode allocation. Both are plain bitmaps stored in journaled
// blocks; per-bitmap-block free counters kept in memory let the allocator skip
// full regions in O(1).

// findZero returns the first clear bit >= start in a bitmap block holding
// nbits valid bits, or -1.
func findZero(b []byte, start, nbits int) int {
	if start >= nbits {
		return -1
	}
	i := start
	// Leading partial byte.
	for ; i < nbits && i&63 != 0; i++ {
		if b[i>>3]&(1<<(i&7)) == 0 {
			return i
		}
	}
	// Whole 64-bit words.
	for ; i+64 <= nbits; i += 64 {
		w := binary.LittleEndian.Uint64(b[i>>3:])
		if w != ^uint64(0) {
			return i + bits.TrailingZeros64(^w)
		}
	}
	for ; i < nbits; i++ {
		if b[i>>3]&(1<<(i&7)) == 0 {
			return i
		}
	}
	return -1
}

func (fs *FS) bbNbits(g int) int {
	n := int(fs.sb.TotalBlocks) - g*format.BitsPerBlock
	return min(n, format.BitsPerBlock)
}

// allocBlock allocates one block, preferring one at or after goal. meta
// allocations may dip into the reserved pool so that metadata operations never
// fail half-way through.
func (fs *FS) allocBlock(goal uint64, meta bool) (uint32, error) {
	if fs.freeBlocks <= fs.reservedBlks && !meta {
		return 0, syscall.ENOSPC
	}
	if fs.freeBlocks == 0 {
		return 0, syscall.ENOSPC
	}
	if goal < fs.sb.DataStart || goal >= fs.sb.TotalBlocks {
		goal = uint64(fs.blockHint)
		if goal < fs.sb.DataStart || goal >= fs.sb.TotalBlocks {
			goal = fs.sb.DataStart
		}
	}
	ng := int(fs.sb.BlockBitmapBlocks)
	g0 := int(goal / format.BitsPerBlock)
	startBit := int(goal % format.BitsPerBlock)
	for k := 0; k <= ng; k++ {
		g := (g0 + k) % ng
		if fs.bbFree[g] == 0 {
			continue
		}
		sb := 0
		if k == 0 {
			sb = startBit
		}
		bm, err := fs.c.get(fs.sb.BlockBitmapStart + uint64(g))
		if err != nil {
			return 0, err
		}
		nbits := fs.bbNbits(g)
		for {
			bit := findZero(bm.data, sb, nbits)
			if bit < 0 {
				break
			}
			blk := uint64(g)*format.BitsPerBlock + uint64(bit)
			if fs.pendingFree(blk) {
				sb = bit + 1
				continue
			}
			bm, err = fs.metaBuf(fs.sb.BlockBitmapStart + uint64(g))
			if err != nil {
				return 0, err
			}
			format.BitSet(bm.data, uint32(bit))
			fs.bbFree[g]--
			fs.freeBlocks--
			fs.blockHint = uint32(blk + 1)
			return uint32(blk), nil
		}
	}
	return 0, syscall.ENOSPC
}

// blockClass says how a freed block was used, which decides whether older
// journal images of it could exist (and so need revoking).
type blockClass int

const (
	classMeta blockClass = iota // indirect, directory, symlink block
	classData                   // regular-file data
)

// freeBlock releases blk. The bitmap bit is cleared inside the running
// transaction but the block is not handed out again until that transaction
// commits (freedPending): otherwise an ordered-mode data write could land on
// a block that the pre-commit on-disk state still treats as metadata.
func (fs *FS) freeBlock(blk uint32, class blockClass) error {
	if fs.snapMask != 0 {
		if done, err := fs.transferToSnapshots(blk, class); err != nil || done {
			return err
		}
	}
	return fs.freeBlockRaw(blk, class)
}

// freeBlockRaw releases blk without consulting snapshots.
func (fs *FS) freeBlockRaw(blk uint32, class blockClass) error {
	b := uint64(blk)
	if b < fs.sb.DataStart || b >= fs.sb.TotalBlocks {
		return corruptf("freeing out-of-range block %d", b)
	}
	g := int(b / format.BitsPerBlock)
	bm, err := fs.metaBuf(fs.sb.BlockBitmapStart + uint64(g))
	if err != nil {
		return err
	}
	bit := uint32(b % format.BitsPerBlock)
	if !format.BitTest(bm.data, bit) {
		return corruptf("double free of block %d", b)
	}
	format.BitClear(bm.data, bit)
	fs.bbFree[g]++
	fs.freeBlocks++
	fs.freedPending[b] = struct{}{}

	// Discard any cached state for the block: it must neither be journaled
	// nor checkpointed any more.
	if old := fs.c.drop(b); old != nil {
		if old.inTxn {
			delete(fs.txMeta, b)
		}
		if old.isData {
			fs.dropFromTxData(old)
		}
		old.inTxn, old.ckpt, old.isData = false, false, false
	}
	// Do NOT discard the block's pending checkpoint image here. The block is
	// only free in the *running* transaction; until that commits, the last
	// durable state still has it allocated and a checkpoint may advance the
	// log tail past the transaction that journaled it, so the image must still
	// reach the home location. finalizeLocked drops it once the freeing
	// transaction is durable.
	if fs.opts.Mode != ModeNone && (class == classMeta || fs.opts.Mode == ModeData) {
		fs.txRevoke[b] = struct{}{}
	}
	return nil
}

func (fs *FS) dropFromTxData(old *buf) {
	for i, x := range fs.txData {
		if x == old {
			fs.txData = append(fs.txData[:i], fs.txData[i+1:]...)
			return
		}
	}
}

// allocInode allocates an inode number.
func (fs *FS) allocInode() (uint32, error) {
	if fs.freeInodes == 0 {
		return 0, syscall.ENOSPC
	}
	ng := int(fs.sb.InodeBitmapBlocks)
	hint := fs.inodeHint
	if hint < 2 || hint >= fs.sb.InodeCount {
		hint = 2
	}
	g0 := int(hint / format.BitsPerBlock)
	startBit := int(hint % format.BitsPerBlock)
	for k := 0; k <= ng; k++ {
		g := (g0 + k) % ng
		if fs.ibFree[g] == 0 {
			continue
		}
		sbit := 0
		if k == 0 {
			sbit = startBit
		}
		bm, err := fs.c.get(fs.sb.InodeBitmapStart + uint64(g))
		if err != nil {
			return 0, err
		}
		nbits := min(int(fs.sb.InodeCount)-g*format.BitsPerBlock, format.BitsPerBlock)
		bit := findZero(bm.data, sbit, nbits)
		if bit < 0 {
			continue
		}
		bm, err = fs.metaBuf(fs.sb.InodeBitmapStart + uint64(g))
		if err != nil {
			return 0, err
		}
		format.BitSet(bm.data, uint32(bit))
		fs.ibFree[g]--
		fs.freeInodes--
		ino := uint32(g*format.BitsPerBlock + bit)
		fs.inodeHint = ino + 1
		return ino, nil
	}
	return 0, syscall.ENOSPC
}

func (fs *FS) freeInodeBit(ino uint32) error {
	g := int(ino / format.BitsPerBlock)
	bm, err := fs.metaBuf(fs.sb.InodeBitmapStart + uint64(g))
	if err != nil {
		return err
	}
	bit := ino % format.BitsPerBlock
	if !format.BitTest(bm.data, bit) {
		return corruptf("double free of inode %d", ino)
	}
	format.BitClear(bm.data, bit)
	fs.ibFree[g]++
	fs.freeInodes++
	if ino < fs.inodeHint {
		fs.inodeHint = ino
	}
	return nil
}

// ---------------------------------------------------------------------------
// Inode table access

func (fs *FS) inodeLoc(ino uint32) (blk uint64, off int) {
	return fs.sb.InodeTableStart + uint64(ino/format.InodesPerBlock),
		int(ino%format.InodesPerBlock) * format.InodeSize
}

func (fs *FS) getInode(ino uint32) (format.Inode, error) {
	if ino == 0 || ino >= fs.sb.InodeCount {
		return format.Inode{}, syscall.ESTALE
	}
	blk, off := fs.inodeLoc(ino)
	b, err := fs.c.get(blk)
	if err != nil {
		return format.Inode{}, syscall.EIO
	}
	in, err := format.DecodeInode(b.data[off:off+format.InodeSize], ino)
	if err != nil {
		return in, syscall.EUCLEAN
	}
	return in, nil
}

func (fs *FS) putInode(ino uint32, in *format.Inode) error {
	blk, off := fs.inodeLoc(ino)
	b, err := fs.metaBuf(blk)
	if err != nil {
		return syscall.EIO
	}
	in.Encode(b.data[off:off+format.InodeSize], ino)
	return nil
}

func corruptf(f string, a ...any) error {
	return &corruptError{msg: sprintf(f, a...)}
}

type corruptError struct{ msg string }

func (e *corruptError) Error() string   { return "walfs: corruption: " + e.msg }
func (e *corruptError) Is(t error) bool { return t == syscall.EUCLEAN }
