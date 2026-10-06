package format

// ---------------------------------------------------------------------------
// Snapshot metadata
//
// A snapshot is a read-only, point-in-time view of the whole filesystem.
//
//   - Creation records the set of blocks that were allocated at that instant
//     (the "captured" bitmap) and costs O(device bitmap), not O(data).
//   - Before a captured block is first overwritten, its old contents are
//     copied to a fresh block P and a record {X -> P, mask} notes which
//     snapshots (bits of mask) see P instead of X. A captured block that is
//     freed rather than overwritten is *transferred*: it stays allocated and
//     the record points to itself (P == X).
//   - Records live in a chain of journaled "COW log" blocks. Deleting a
//     snapshot clears its bit in every mask and releases blocks whose mask
//     becomes empty.
//
// Only inode-table and data-area blocks are ever preserved: the superblock,
// journal and both bitmaps are never part of a snapshot's view.

const (
	MaxSnapshots = 32
	SnapNameMax  = 95

	snapTableMagic = 0x54504E53 // "SNPT"
	snapCapMagic   = 0x5041434E // "NCAP"
	cowMagic       = 0x4C574F43 // "COWL"

	snapEntrySize = 120
	snapEntryOff  = 16
	snapCRCOff    = BlockSize - 4

	// CowRecsPerBlock is the number of records in one COW log block.
	CowRecsPerBlock = 255
	cowRecOff       = 8
	// MaxCapBlocks is the most captured-bitmap blocks a snapshot may have.
	MaxCapBlocks = (BlockSize - 12) / 4
)

// Snapshot states in the table.
const (
	SnapFree     = 0
	SnapActive   = 1
	SnapDeleting = 2
)

// SnapEntry is one slot of the snapshot table.
type SnapEntry struct {
	State     uint32
	CapList   uint32 // block listing the captured-bitmap blocks
	CapBlocks uint32
	CreatedNS int64
	Name      string
}

// SnapTable is the decoded snapshot table block.
type SnapTable struct {
	Entries [MaxSnapshots]SnapEntry
}

func (t *SnapTable) Marshal() []byte {
	b := make([]byte, BlockSize)
	le.PutUint32(b[0:], snapTableMagic)
	for i, e := range t.Entries {
		o := snapEntryOff + i*snapEntrySize
		le.PutUint32(b[o:], e.State)
		le.PutUint32(b[o+4:], e.CapList)
		le.PutUint64(b[o+8:], uint64(e.CreatedNS))
		copy(b[o+16:o+16+96], e.Name)
		le.PutUint32(b[o+112:], e.CapBlocks)
	}
	le.PutUint32(b[snapCRCOff:], CRC32C(0, b[:snapCRCOff]))
	return b
}

func ParseSnapTable(b []byte) (*SnapTable, error) {
	if le.Uint32(b[0:]) != snapTableMagic {
		return nil, corrupt("bad snapshot table magic")
	}
	if le.Uint32(b[snapCRCOff:]) != CRC32C(0, b[:snapCRCOff]) {
		return nil, corrupt("snapshot table checksum mismatch")
	}
	t := &SnapTable{}
	for i := range t.Entries {
		o := snapEntryOff + i*snapEntrySize
		name := b[o+16 : o+16+96]
		n := 0
		for n < len(name) && name[n] != 0 {
			n++
		}
		t.Entries[i] = SnapEntry{
			State: le.Uint32(b[o:]), CapList: le.Uint32(b[o+4:]),
			CreatedNS: int64(le.Uint64(b[o+8:])), CapBlocks: le.Uint32(b[o+112:]),
			Name: string(name[:n]),
		}
		if st := t.Entries[i].State; st > SnapDeleting {
			return nil, corrupt("snapshot slot %d has invalid state %d", i, st)
		}
	}
	return t, nil
}

// EncodeCapList builds the block that lists a snapshot's captured-bitmap blocks.
func EncodeCapList(blocks []uint32) []byte {
	b := make([]byte, BlockSize)
	le.PutUint32(b[0:], snapCapMagic)
	le.PutUint32(b[4:], uint32(len(blocks)))
	for i, p := range blocks {
		le.PutUint32(b[8+4*i:], p)
	}
	le.PutUint32(b[snapCRCOff:], CRC32C(0, b[:snapCRCOff]))
	return b
}

func ParseCapList(b []byte) ([]uint32, error) {
	if le.Uint32(b[0:]) != snapCapMagic {
		return nil, corrupt("bad capture list magic")
	}
	if le.Uint32(b[snapCRCOff:]) != CRC32C(0, b[:snapCRCOff]) {
		return nil, corrupt("capture list checksum mismatch")
	}
	n := int(le.Uint32(b[4:]))
	if n > MaxCapBlocks {
		return nil, corrupt("capture list too long")
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = le.Uint32(b[8+4*i:])
	}
	return out, nil
}

// COW record flags.
const (
	CowTransferred = 1 << 0 // P == X: the block itself now belongs to the snapshots
	CowMeta        = 1 << 1 // the block held journaled metadata (matters when it is finally freed)
)

// CowRec says: snapshots in Mask see physical block P wherever they refer to X.
type CowRec struct {
	X, P, Mask, Flags uint32
}

func (r CowRec) Empty() bool { return r.Mask == 0 }

// CowBlock is a decoded COW log block.
type CowBlock struct {
	Next uint32
	Recs [CowRecsPerBlock]CowRec
}

func (c *CowBlock) Marshal() []byte {
	b := make([]byte, BlockSize)
	EncodeCow(b, c)
	return b
}

// EncodeCow writes c into b (a full block), including its checksum.
func EncodeCow(b []byte, c *CowBlock) {
	clear(b)
	le.PutUint32(b[0:], cowMagic)
	le.PutUint32(b[4:], c.Next)
	for i, r := range c.Recs {
		o := cowRecOff + 16*i
		le.PutUint32(b[o:], r.X)
		le.PutUint32(b[o+4:], r.P)
		le.PutUint32(b[o+8:], r.Mask)
		le.PutUint32(b[o+12:], r.Flags)
	}
	le.PutUint32(b[snapCRCOff:], CRC32C(0, b[:snapCRCOff]))
}

func ParseCow(b []byte) (*CowBlock, error) {
	if le.Uint32(b[0:]) != cowMagic {
		return nil, corrupt("bad COW log magic")
	}
	if le.Uint32(b[snapCRCOff:]) != CRC32C(0, b[:snapCRCOff]) {
		return nil, corrupt("COW log checksum mismatch")
	}
	c := &CowBlock{Next: le.Uint32(b[4:])}
	for i := range c.Recs {
		o := cowRecOff + 16*i
		c.Recs[i] = CowRec{X: le.Uint32(b[o:]), P: le.Uint32(b[o+4:]), Mask: le.Uint32(b[o+8:]), Flags: le.Uint32(b[o+12:])}
	}
	return c, nil
}

// Preservable reports whether blocks at or after the inode table can be part of
// a snapshot's view.
func (s *Superblock) Preservable(blk uint64) bool {
	return blk >= s.InodeTableStart && blk < s.TotalBlocks
}

// CowPut stores record r in slot idx of the COW log block b and refreshes the
// block checksum.
func CowPut(b []byte, idx int, r CowRec) {
	o := cowRecOff + 16*idx
	le.PutUint32(b[o:], r.X)
	le.PutUint32(b[o+4:], r.P)
	le.PutUint32(b[o+8:], r.Mask)
	le.PutUint32(b[o+12:], r.Flags)
	le.PutUint32(b[snapCRCOff:], CRC32C(0, b[:snapCRCOff]))
}

// CowSetNext links block b to the next COW log block.
func CowSetNext(b []byte, next uint32) {
	le.PutUint32(b[4:], next)
	le.PutUint32(b[snapCRCOff:], CRC32C(0, b[:snapCRCOff]))
}
