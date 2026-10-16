package format

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/Vinayak069/walfs/internal/blockdev"
)

// MkfsOptions controls filesystem creation.
type MkfsOptions struct {
	Inodes        uint32 // 0: one inode per 16 KiB
	JournalBlocks uint64 // 0: size/32 clamped to [128, 32768] blocks
	AssumeZero    bool   // device is freshly created/sparse: skip zeroing the journal
	Now           int64  // 0: time.Now()
	RootUID       uint32 // owner of the root directory
	RootGID       uint32
}

// Geometry computes the superblock for a device of the given size.
func Geometry(sizeBytes int64, o MkfsOptions) (*Superblock, error) {
	total := uint64(sizeBytes / BlockSize)
	if total < 64 {
		return nil, fmt.Errorf("mkfs: device too small (%d blocks)", total)
	}
	sb := &Superblock{
		Magic: SBMagic, Version: Version, BlockSize: BlockSize, InodeSize: InodeSize,
		TotalBlocks: total,
	}
	ic := uint64(o.Inodes)
	if ic == 0 {
		ic = total / 4
	}
	if ic < 64 {
		ic = 64
	}
	ic = (ic + InodesPerBlock - 1) &^ (InodesPerBlock - 1)
	if ic > 1<<31 {
		ic = 1 << 31
	}
	sb.InodeCount = uint32(ic)
	jb := o.JournalBlocks
	if jb == 0 {
		jb = total / 32
		if jb < 128 {
			jb = 128
		}
		if jb > 32768 {
			jb = 32768
		}
	}
	sb.JournalStart = 1
	sb.JournalBlocks = jb
	sb.InodeBitmapStart = sb.JournalStart + sb.JournalBlocks
	sb.InodeBitmapBlocks = (ic + BitsPerBlock - 1) / BitsPerBlock
	sb.BlockBitmapStart = sb.InodeBitmapStart + sb.InodeBitmapBlocks
	sb.BlockBitmapBlocks = (total + BitsPerBlock - 1) / BitsPerBlock
	sb.InodeTableStart = sb.BlockBitmapStart + sb.BlockBitmapBlocks
	sb.InodeTableBlocks = ic / InodesPerBlock
	sb.DataStart = sb.InodeTableStart + sb.InodeTableBlocks
	if sb.DataStart+16 > total {
		return nil, fmt.Errorf("mkfs: device too small for %d inodes and a %d-block journal", ic, jb)
	}
	return sb, sb.Validate()
}

// Mkfs creates an empty filesystem (just the root directory) on dev.
func Mkfs(dev blockdev.Device, o MkfsOptions) (*Superblock, error) {
	sb, err := Geometry(dev.Size(), o)
	if err != nil {
		return nil, err
	}
	now := o.Now
	if now == 0 {
		now = time.Now().UnixNano()
	}
	if _, err := rand.Read(sb.UUID[:]); err != nil {
		return nil, err
	}
	sb.CreatedNS = now
	w := func(blk uint64, b []byte) error {
		_, err := dev.WriteAt(b, int64(blk)*BlockSize)
		return err
	}
	zero := make([]byte, BlockSize)

	// Journal: superblock, plus (unless the device is known zero) a scrubbed log.
	if !o.AssumeZero {
		for b := uint64(0); b < sb.JournalBlocks; b++ {
			if err := w(sb.JournalStart+b, zero); err != nil {
				return nil, err
			}
		}
	}
	js := JournalSuper{Blocks: sb.JournalBlocks, TailOff: 0, TailSeq: 1}
	if err := w(sb.JournalStart, js.Marshal()); err != nil {
		return nil, err
	}

	// Inode bitmap: slot 0 is invalid, slot 1 is the root; padding bits set.
	for i := uint64(0); i < sb.InodeBitmapBlocks; i++ {
		b := make([]byte, BlockSize)
		for bit := uint64(0); bit < BitsPerBlock; bit++ {
			ino := i*BitsPerBlock + bit
			if ino <= RootIno || ino >= uint64(sb.InodeCount) {
				BitSet(b, uint32(bit))
			}
		}
		if err := w(sb.InodeBitmapStart+i, b); err != nil {
			return nil, err
		}
	}

	// Block bitmap: all metadata blocks, the root directory block, and padding.
	rootBlk := sb.DataStart
	for i := uint64(0); i < sb.BlockBitmapBlocks; i++ {
		b := make([]byte, BlockSize)
		for bit := uint64(0); bit < BitsPerBlock; bit++ {
			blk := i*BitsPerBlock + bit
			if blk <= rootBlk || blk >= sb.TotalBlocks {
				BitSet(b, uint32(bit))
			}
		}
		if err := w(sb.BlockBitmapStart+i, b); err != nil {
			return nil, err
		}
	}

	// Inode table block holding the root inode (the rest is lazily initialised:
	// an inode slot is only ever read after its bitmap bit is set).
	tbl := make([]byte, BlockSize)
	root := Inode{
		Mode: SIFDIR | 0o755, UID: o.RootUID, GID: o.RootGID, Nlink: 2, Size: BlockSize, Blocks: 1,
		Atime: now, Mtime: now, Ctime: now, Crtime: now, Gen: 1,
	}
	root.Direct[0] = uint32(rootBlk)
	root.Encode(tbl[RootIno*InodeSize:], RootIno)
	if err := w(sb.InodeTableStart, tbl); err != nil {
		return nil, err
	}

	// Root directory block: "." and "..", the last entry spans the block.
	db := make([]byte, BlockSize)
	PutDirent(db, 0, RootIno, DirentLen(1), FTDir, ".")
	PutDirent(db, DirentLen(1), RootIno, BlockSize-DirentLen(1), FTDir, "..")
	if err := w(rootBlk, db); err != nil {
		return nil, err
	}

	if err := dev.Flush(); err != nil {
		return nil, err
	}
	// The superblock goes last: a half-made filesystem has no valid magic.
	if err := w(0, sb.Marshal()); err != nil {
		return nil, err
	}
	return sb, dev.Flush()
}
