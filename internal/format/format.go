// Package format defines every on-disk structure of walfs and the code that
// (de)serialises it, including journal recovery. It is shared by the
// filesystem engine, mkfs and fsck so that all three agree on the format.
//
// Disk layout (all integers little-endian, block size 4096):
//
//	block 0                    superblock (fits in one 512-byte sector)
//	JournalStart               journal superblock, then the circular log
//	InodeBitmapStart           inode allocation bitmap
//	BlockBitmapStart           block allocation bitmap (one bit per device block)
//	InodeTableStart            256-byte inodes, inode number == table slot
//	DataStart ...              file data, directory blocks, indirect blocks
package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

const (
	BlockSize      = 4096
	SectorSize     = 512
	InodeSize      = 256
	InodesPerBlock = BlockSize / InodeSize
	NDirect        = 12
	PtrsPerBlock   = BlockSize / 4
	RootIno        = 1
	NameMax        = 255
	MaxLinks       = 65000
	BitsPerBlock   = BlockSize * 8

	Version = 1
)

// File-type bits of st_mode (same values as <sys/stat.h>).
const (
	SIFMT   = 0o170000
	SIFSOCK = 0o140000
	SIFLNK  = 0o120000
	SIFREG  = 0o100000
	SIFBLK  = 0o060000
	SIFDIR  = 0o040000
	SIFCHR  = 0o020000
	SIFIFO  = 0o010000
)

// Directory-entry type codes (same values as DT_*).
const (
	FTUnknown = 0
	FTFifo    = 1
	FTChr     = 2
	FTDir     = 4
	FTBlk     = 6
	FTReg     = 8
	FTLnk     = 10
	FTSock    = 12
)

// FTypeOf maps st_mode to a directory-entry type.
func FTypeOf(mode uint32) uint8 {
	switch mode & SIFMT {
	case SIFREG:
		return FTReg
	case SIFDIR:
		return FTDir
	case SIFLNK:
		return FTLnk
	case SIFCHR:
		return FTChr
	case SIFBLK:
		return FTBlk
	case SIFIFO:
		return FTFifo
	case SIFSOCK:
		return FTSock
	}
	return FTUnknown
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// CRC32C computes a Castagnoli CRC, the same polynomial ext4/jbd2 use.
func CRC32C(seed uint32, b []byte) uint32 { return crc32.Update(seed, castagnoli, b) }

var le = binary.LittleEndian

// ErrCorrupt wraps all structural validation failures.
var ErrCorrupt = errors.New("walfs: corrupt on-disk structure")

func corrupt(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, a...))
}

// ---------------------------------------------------------------------------
// Superblock

const SBMagic = 0x31305653464C4157 // the bytes "WALFSV01" read as a little-endian u64

// Superblock describes the geometry of the filesystem. Only OrphanHead ever
// changes after mkfs; it is updated through the journal.
type Superblock struct {
	Magic             uint64
	Version           uint32
	BlockSize         uint32
	TotalBlocks       uint64
	InodeCount        uint32 // number of inode slots, including reserved slot 0
	InodeSize         uint32
	JournalStart      uint64
	JournalBlocks     uint64 // including the journal superblock
	InodeBitmapStart  uint64
	InodeBitmapBlocks uint64
	BlockBitmapStart  uint64
	BlockBitmapBlocks uint64
	InodeTableStart   uint64
	InodeTableBlocks  uint64
	DataStart         uint64
	OrphanHead        uint32 // head of the singly linked list of unlinked-but-open inodes
	UUID              [16]byte
	CreatedNS         int64
}

const sbCRCOff = SectorSize - 4

// Marshal serialises the superblock into a full block. The structure and its
// CRC live entirely in the first sector, so updates are sector-atomic.
func (s *Superblock) Marshal() []byte {
	b := make([]byte, BlockSize)
	le.PutUint64(b[0:], s.Magic)
	le.PutUint32(b[8:], s.Version)
	le.PutUint32(b[12:], s.BlockSize)
	le.PutUint64(b[16:], s.TotalBlocks)
	le.PutUint32(b[24:], s.InodeCount)
	le.PutUint32(b[28:], s.InodeSize)
	le.PutUint64(b[32:], s.JournalStart)
	le.PutUint64(b[40:], s.JournalBlocks)
	le.PutUint64(b[48:], s.InodeBitmapStart)
	le.PutUint64(b[56:], s.InodeBitmapBlocks)
	le.PutUint64(b[64:], s.BlockBitmapStart)
	le.PutUint64(b[72:], s.BlockBitmapBlocks)
	le.PutUint64(b[80:], s.InodeTableStart)
	le.PutUint64(b[88:], s.InodeTableBlocks)
	le.PutUint64(b[96:], s.DataStart)
	le.PutUint32(b[104:], s.OrphanHead)
	copy(b[108:124], s.UUID[:])
	le.PutUint64(b[128:], uint64(s.CreatedNS))
	le.PutUint32(b[sbCRCOff:], CRC32C(0, b[:sbCRCOff]))
	return b
}

// UnmarshalSuperblock parses and validates block 0.
func UnmarshalSuperblock(b []byte) (*Superblock, error) {
	if len(b) < SectorSize {
		return nil, corrupt("short superblock")
	}
	if le.Uint64(b[0:]) != SBMagic {
		return nil, corrupt("bad superblock magic")
	}
	if got, want := le.Uint32(b[sbCRCOff:]), CRC32C(0, b[:sbCRCOff]); got != want {
		return nil, corrupt("superblock checksum mismatch (%08x != %08x)", got, want)
	}
	s := &Superblock{
		Magic:             le.Uint64(b[0:]),
		Version:           le.Uint32(b[8:]),
		BlockSize:         le.Uint32(b[12:]),
		TotalBlocks:       le.Uint64(b[16:]),
		InodeCount:        le.Uint32(b[24:]),
		InodeSize:         le.Uint32(b[28:]),
		JournalStart:      le.Uint64(b[32:]),
		JournalBlocks:     le.Uint64(b[40:]),
		InodeBitmapStart:  le.Uint64(b[48:]),
		InodeBitmapBlocks: le.Uint64(b[56:]),
		BlockBitmapStart:  le.Uint64(b[64:]),
		BlockBitmapBlocks: le.Uint64(b[72:]),
		InodeTableStart:   le.Uint64(b[80:]),
		InodeTableBlocks:  le.Uint64(b[88:]),
		DataStart:         le.Uint64(b[96:]),
		OrphanHead:        le.Uint32(b[104:]),
		CreatedNS:         int64(le.Uint64(b[128:])),
	}
	copy(s.UUID[:], b[108:124])
	if s.Version != Version {
		return nil, corrupt("unsupported version %d", s.Version)
	}
	if s.BlockSize != BlockSize || s.InodeSize != InodeSize {
		return nil, corrupt("unsupported block/inode size %d/%d", s.BlockSize, s.InodeSize)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Validate checks that the regions are in order, non-overlapping and in range.
func (s *Superblock) Validate() error {
	type reg struct {
		name       string
		start, len uint64
	}
	regs := []reg{
		{"journal", s.JournalStart, s.JournalBlocks},
		{"inode bitmap", s.InodeBitmapStart, s.InodeBitmapBlocks},
		{"block bitmap", s.BlockBitmapStart, s.BlockBitmapBlocks},
		{"inode table", s.InodeTableStart, s.InodeTableBlocks},
	}
	next := uint64(1)
	for _, r := range regs {
		if r.start != next {
			return corrupt("%s starts at %d, expected %d", r.name, r.start, next)
		}
		next = r.start + r.len
	}
	if s.DataStart != next || s.DataStart >= s.TotalBlocks {
		return corrupt("data start %d inconsistent (regions end at %d, total %d)", s.DataStart, next, s.TotalBlocks)
	}
	if s.TotalBlocks > 1<<32-1 {
		return corrupt("device too large")
	}
	if uint64(s.InodeCount) > s.InodeBitmapBlocks*BitsPerBlock ||
		uint64(s.InodeCount) > s.InodeTableBlocks*InodesPerBlock {
		return corrupt("inode count %d exceeds table/bitmap capacity", s.InodeCount)
	}
	if s.TotalBlocks > s.BlockBitmapBlocks*BitsPerBlock {
		return corrupt("block bitmap too small")
	}
	if s.JournalBlocks < 96 {
		return corrupt("journal too small")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Inode

// Inode is the in-memory form of a 256-byte on-disk inode.
type Inode struct {
	Mode       uint32
	UID, GID   uint32
	Nlink      uint32
	Size       uint64
	Blocks     uint64 // allocated blocks (data + indirect), in BlockSize units
	Atime      int64  // nanoseconds since the epoch
	Mtime      int64
	Ctime      int64
	Crtime     int64
	Gen        uint32
	OrphanNext uint32
	Rdev       uint64
	Flags      uint32
	Direct     [NDirect]uint32
	Ind        uint32
	DInd       uint32
	TInd       uint32
	Inline     []byte // symlink target when InlineSymlink()
}

const (
	inoDirectOff = 88
	inoCRCOff    = InodeSize - 4
	// InlineSymlinkMax is the longest symlink target stored inside the inode
	// (reusing the block-pointer area, as ext2/3/4 "fast symlinks" do).
	InlineSymlinkMax = 4 * (NDirect + 3) // 60
)

func (i *Inode) IsDir() bool { return i.Mode&SIFMT == SIFDIR }
func (i *Inode) IsReg() bool { return i.Mode&SIFMT == SIFREG }
func (i *Inode) IsLnk() bool { return i.Mode&SIFMT == SIFLNK }

// InlineSymlink reports whether the target of this symlink lives in the inode.
func (i *Inode) InlineSymlink() bool { return i.IsLnk() && i.Size <= InlineSymlinkMax && i.Blocks == 0 }

// Encode writes the inode (and its checksum, which is seeded with the inode
// number so a stale inode copied to the wrong slot is detected).
func (i *Inode) Encode(b []byte, ino uint32) {
	for k := range b[:InodeSize] {
		b[k] = 0
	}
	le.PutUint32(b[0:], i.Mode)
	le.PutUint32(b[4:], i.UID)
	le.PutUint32(b[8:], i.GID)
	le.PutUint32(b[12:], i.Nlink)
	le.PutUint64(b[16:], i.Size)
	le.PutUint64(b[24:], i.Blocks)
	le.PutUint64(b[32:], uint64(i.Atime))
	le.PutUint64(b[40:], uint64(i.Mtime))
	le.PutUint64(b[48:], uint64(i.Ctime))
	le.PutUint64(b[56:], uint64(i.Crtime))
	le.PutUint32(b[64:], i.Gen)
	le.PutUint32(b[68:], i.OrphanNext)
	le.PutUint64(b[72:], i.Rdev)
	le.PutUint32(b[80:], i.Flags)
	if i.InlineSymlink() {
		copy(inlineArea(b), i.Inline)
	} else {
		for k, p := range i.Direct {
			le.PutUint32(b[inoDirectOff+4*k:], p)
		}
		le.PutUint32(b[inoDirectOff+4*NDirect:], i.Ind)
		le.PutUint32(b[inoDirectOff+4*NDirect+4:], i.DInd)
		le.PutUint32(b[inoDirectOff+4*NDirect+8:], i.TInd)
	}
	le.PutUint32(b[inoCRCOff:], inodeCRC(b, ino))
}

func inlineArea(b []byte) []byte { return b[inoDirectOff : inoDirectOff+InlineSymlinkMax] }

func inodeCRC(b []byte, ino uint32) uint32 {
	var s [4]byte
	le.PutUint32(s[:], ino)
	return CRC32C(CRC32C(0, s[:]), b[:inoCRCOff])
}

// DecodeInode parses an inode. A completely zero slot decodes to the zero
// Inode with ok=true (an unallocated inode); anything else must carry a valid
// checksum.
func DecodeInode(b []byte, ino uint32) (Inode, error) {
	var i Inode
	b = b[:InodeSize]
	if le.Uint32(b[inoCRCOff:]) != inodeCRC(b, ino) {
		allZero := true
		for _, c := range b {
			if c != 0 {
				allZero = false
				break
			}
		}
		if allZero {
			return i, nil
		}
		return i, corrupt("inode %d checksum mismatch", ino)
	}
	i.Mode = le.Uint32(b[0:])
	i.UID = le.Uint32(b[4:])
	i.GID = le.Uint32(b[8:])
	i.Nlink = le.Uint32(b[12:])
	i.Size = le.Uint64(b[16:])
	i.Blocks = le.Uint64(b[24:])
	i.Atime = int64(le.Uint64(b[32:]))
	i.Mtime = int64(le.Uint64(b[40:]))
	i.Ctime = int64(le.Uint64(b[48:]))
	i.Crtime = int64(le.Uint64(b[56:]))
	i.Gen = le.Uint32(b[64:])
	i.OrphanNext = le.Uint32(b[68:])
	i.Rdev = le.Uint64(b[72:])
	i.Flags = le.Uint32(b[80:])
	if i.InlineSymlink() {
		i.Inline = append([]byte(nil), inlineArea(b)[:i.Size]...)
	} else {
		for k := range i.Direct {
			i.Direct[k] = le.Uint32(b[inoDirectOff+4*k:])
		}
		i.Ind = le.Uint32(b[inoDirectOff+4*NDirect:])
		i.DInd = le.Uint32(b[inoDirectOff+4*NDirect+4:])
		i.TInd = le.Uint32(b[inoDirectOff+4*NDirect+8:])
	}
	return i, nil
}

// ---------------------------------------------------------------------------
// Directory entries (ext2-style: variable length, chained by rec_len)

const DirentHdr = 8

// DirentLen is the minimum record length for a name of n bytes (4-aligned).
func DirentLen(n int) int { return (DirentHdr + n + 3) &^ 3 }

// Dirent is a decoded directory entry.
type Dirent struct {
	Ino    uint32
	RecLen uint16
	FType  uint8
	Name   string
}

// ParseDirent decodes the entry at b[off:], validating its framing.
func ParseDirent(b []byte, off int) (Dirent, error) {
	if off+DirentHdr > len(b) {
		return Dirent{}, corrupt("dirent header past block end at %d", off)
	}
	d := Dirent{
		Ino:    le.Uint32(b[off:]),
		RecLen: le.Uint16(b[off+4:]),
		FType:  b[off+7],
	}
	nl := int(b[off+6])
	rl := int(d.RecLen)
	if rl < DirentHdr || rl%4 != 0 || off+rl > len(b) || DirentHdr+nl > rl {
		return d, corrupt("bad dirent at %d: rec_len=%d name_len=%d", off, rl, nl)
	}
	if d.Ino != 0 {
		d.Name = string(b[off+DirentHdr : off+DirentHdr+nl])
	}
	return d, nil
}

// PutDirent writes an entry header and name at b[off:] with the given rec_len.
func PutDirent(b []byte, off int, ino uint32, recLen int, ftype uint8, name string) {
	le.PutUint32(b[off:], ino)
	le.PutUint16(b[off+4:], uint16(recLen))
	b[off+6] = uint8(len(name))
	b[off+7] = ftype
	copy(b[off+DirentHdr:], name)
}

// SetRecLen rewrites only the rec_len field of the entry at off.
func SetRecLen(b []byte, off, recLen int) { le.PutUint16(b[off+4:], uint16(recLen)) }

// ClearIno marks the entry at off as unused.
func ClearIno(b []byte, off int) { le.PutUint32(b[off:], 0) }

// ---------------------------------------------------------------------------
// Bitmaps

func BitTest(b []byte, i uint32) bool { return b[i>>3]&(1<<(i&7)) != 0 }
func BitSet(b []byte, i uint32)       { b[i>>3] |= 1 << (i & 7) }
func BitClear(b []byte, i uint32)     { b[i>>3] &^= 1 << (i & 7) }

// IBits is the number of bits in the inode bitmap region.
func (s *Superblock) IBits() uint64 { return s.InodeBitmapBlocks * BitsPerBlock }
