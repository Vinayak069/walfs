package fsck

import (
	"fmt"

	"github.com/Vinayak069/walfs/internal/blockdev"
	"github.com/Vinayak069/walfs/internal/format"
)

// Owner tags used for blocks that belong to no inode.
const (
	ownerSnapMeta = 0xFFFFFFF0 // snapshot table, capture lists/bitmaps, COW log
	ownerSnapData = 0xFFFFFFF1 // preserved copies and blocks transferred to snapshots
)

// snapRec is a decoded COW record.
type snapRec struct{ x, p, mask, flags uint32 }

// checkSnapshots verifies the snapshot machinery and then every snapshot view
// as a filesystem in its own right.
func (c *checker) checkSnapshots() {
	sb := c.sb
	if sb.SnapTable == 0 {
		if sb.CowHead != 0 {
			c.errf("COW log head set but there is no snapshot table")
		}
		return
	}
	tb, err := c.readBlock(uint64(sb.SnapTable))
	if err != nil {
		c.errf("snapshot table unreadable: %v", err)
		return
	}
	tbl, err := format.ParseSnapTable(tb)
	if err != nil {
		c.errf("snapshot table: %v", err)
		return
	}
	c.claim(sb.SnapTable, ownerSnapMeta, "snapshot table")
	nbb := int(sb.BlockBitmapBlocks)

	type snap struct {
		slot   int
		e      format.SnapEntry
		bits   [][]byte
		active bool
	}
	var snaps []*snap
	valid := uint32(0) // slots that exist
	names := map[string]bool{}
	for i, e := range tbl.Entries {
		if e.State == format.SnapFree {
			continue
		}
		if names[e.Name] {
			c.errf("snapshot name %q used twice", e.Name)
		}
		names[e.Name] = true
		valid |= 1 << i
		s := &snap{slot: i, e: e, active: e.State == format.SnapActive}
		lb, err := c.readBlock(uint64(e.CapList))
		if err != nil {
			c.errf("snapshot %q: capture list unreadable: %v", e.Name, err)
			continue
		}
		blocks, err := format.ParseCapList(lb)
		if err != nil {
			c.errf("snapshot %q: %v", e.Name, err)
			continue
		}
		c.claim(e.CapList, ownerSnapMeta, "snapshot capture list")
		if len(blocks) != nbb {
			c.errf("snapshot %q: capture bitmap has %d blocks, expected %d", e.Name, len(blocks), nbb)
			continue
		}
		for _, cb := range blocks {
			if !c.claim(cb, ownerSnapMeta, "snapshot capture bitmap") {
				continue
			}
			b, err := c.readBlock(uint64(cb))
			if err != nil {
				c.errf("snapshot %q: capture bitmap unreadable: %v", e.Name, err)
				continue
			}
			s.bits = append(s.bits, b)
		}
		snaps = append(snaps, s)
	}
	c.rep.Stats.Snapshots = len(snaps)

	// COW record log.
	var recs []snapRec
	for blk, n := sb.CowHead, 0; blk != 0; n++ {
		if n > int(sb.TotalBlocks) {
			c.errf("COW log chain is cyclic")
			break
		}
		if !c.claim(blk, ownerSnapMeta, "COW log") {
			break
		}
		b, err := c.readBlock(uint64(blk))
		if err != nil {
			c.errf("COW log block %d unreadable: %v", blk, err)
			break
		}
		cb, err := format.ParseCow(b)
		if err != nil {
			c.errf("COW log block %d: %v", blk, err)
			break
		}
		for _, r := range cb.Recs {
			if !r.Empty() {
				recs = append(recs, snapRec{r.X, r.P, r.Mask, r.Flags})
			}
		}
		blk = cb.Next
	}
	c.rep.Stats.CowRecords = len(recs)

	seenMask := map[uint32]uint32{} // X -> union of masks, to detect overlap
	for _, r := range recs {
		switch {
		case !sb.Preservable(uint64(r.x)):
			c.errf("COW record for block %d, which can never be part of a snapshot", r.x)
			continue
		case uint64(r.p) < sb.DataStart || uint64(r.p) >= sb.TotalBlocks:
			c.errf("COW record %d->%d: preserved block outside the data area", r.x, r.p)
			continue
		case r.mask&^valid != 0:
			c.errf("COW record %d->%d: mask %#x names a non-existent snapshot", r.x, r.p, r.mask)
		case r.flags&format.CowTransferred != 0 && r.p != r.x:
			c.errf("COW record %d->%d is marked transferred but P != X", r.x, r.p)
		case r.flags&format.CowTransferred == 0 && r.p == r.x:
			c.errf("COW record for block %d points to itself without the transferred flag", r.x)
		}
		if seenMask[r.x]&r.mask != 0 {
			c.errf("block %d has two COW records for the same snapshot", r.x)
		}
		seenMask[r.x] |= r.mask
		if c.claim(r.p, ownerSnapData, fmt.Sprintf("preserved copy of block %d", r.x)) {
			c.rep.Stats.SnapBlocks++
		}
	}

	// Each snapshot must be a consistent filesystem.
	for _, s := range snaps {
		if !s.active || len(s.bits) != nbb {
			continue
		}
		c.checkView(s.slot, s.e.Name, s.bits, recs)
	}
}

// viewDev presents a snapshot as a block device: preserved copies where a
// record says so, the live block otherwise.
type viewDev struct {
	raw  blockdev.Device
	xmap map[uint32]uint32
}

func (d *viewDev) ReadAt(p []byte, off int64) (int, error) {
	for done := 0; done < len(p); done += format.BlockSize {
		blk := uint32((off + int64(done)) / format.BlockSize)
		if q, ok := d.xmap[blk]; ok {
			blk = q
		}
		if _, err := d.raw.ReadAt(p[done:done+format.BlockSize], int64(blk)*format.BlockSize); err != nil {
			return done, err
		}
	}
	return len(p), nil
}
func (d *viewDev) WriteAt(p []byte, off int64) (int, error) { return 0, fmt.Errorf("read-only") }
func (d *viewDev) Flush() error                             { return nil }
func (d *viewDev) Size() int64                              { return d.raw.Size() }
func (d *viewDev) Close() error                             { return nil }

func (c *checker) checkView(slot int, name string, bits [][]byte, recs []snapRec) {
	xmap := map[uint32]uint32{}
	for _, r := range recs {
		if r.mask&(1<<slot) != 0 {
			xmap[r.x] = r.p
		}
	}
	v := &checker{
		dev: &viewDev{raw: c.dev, xmap: xmap}, sb: c.sb, rep: &Report{}, max: c.max,
		owner: map[uint32]uint32{}, inodes: map[uint32]*info{},
		refs: map[uint32]uint32{}, subdirs: map[uint32]uint32{},
		parent: map[uint32]uint32{}, dirOf: map[uint32]uint32{}, view: true,
	}
	if err := v.scanInodes(); err != nil {
		c.errf("snapshot %q: %v", name, err)
		return
	}
	v.checkDirs()
	v.checkLinks()
	for _, e := range v.rep.Errors {
		c.errf("snapshot %q: %s", name, e)
	}
	// Every block the snapshot references must (a) have been allocated when
	// the snapshot was taken and (b) still exist physically, either unchanged
	// or as a preserved copy.
	for x := range v.owner {
		if !format.BitTest(bits[x/format.BitsPerBlock], x%format.BitsPerBlock) {
			c.errf("snapshot %q references block %d which was not allocated at capture time", name, x)
			continue
		}
		phys := x
		if q, ok := xmap[x]; ok {
			phys = q
			if o := c.owner[q]; o != ownerSnapData {
				c.errf("snapshot %q: preserved copy %d of block %d is not owned by the snapshot machinery", name, q, x)
			}
		} else if !format.BitTest(c.bbmap, phys) {
			c.errf("snapshot %q references block %d which has been freed without being preserved", name, x)
		} else if o, ok := c.owner[phys]; !ok || o >= ownerSnapMeta {
			c.errf("snapshot %q references block %d which is not owned by the live filesystem (owner %#x)", name, x, o)
		}
	}
}
