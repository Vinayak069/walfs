// Package fsck is an offline consistency checker for walfs images.
//
// It is deliberately independent of the engine: it re-implements block-map
// traversal and directory parsing on top of internal/format only, so a bug in
// the engine's allocator, journal or directory code cannot hide itself from
// the checker. It verifies, among others:
//
//   - superblock / journal-superblock / inode checksums and geometry
//   - every block pointer is in range and owned by exactly one inode
//   - block and inode bitmaps match exactly the set of reachable objects
//   - directory structure: rec_len chains, '.' and '..', unique names,
//     entry types, one parent per directory, no cycles, all reachable
//   - link counts of every inode against the number of references found
//   - the orphan list (nlink==0 inodes) is well formed and complete
//   - i_blocks accounting and size/pointer consistency per file type
package fsck

import (
	"fmt"
	"slices"
	"strings"

	"github.com/vinayak-singh/walfs/internal/blockdev"
	"github.com/vinayak-singh/walfs/internal/format"
)

// Report is the result of a check.
type Report struct {
	Errors []string
	Stats  Stats
}

// Stats summarises the filesystem contents.
type Stats struct {
	Inodes, Dirs, Files, Symlinks, Specials uint64
	BlocksUsed, MetaBlocks                  uint64
	Orphans                                 int
	JournalTxns                             int // committed transactions still in the log (unreplayed)
	Snapshots, CowRecords, SnapBlocks       int
}

// OK reports whether no inconsistency was found.
func (r *Report) OK() bool { return len(r.Errors) == 0 }

func (r *Report) String() string {
	var sb strings.Builder
	if r.OK() {
		sb.WriteString("clean: ")
	} else {
		fmt.Fprintf(&sb, "%d error(s):\n", len(r.Errors))
		for _, e := range r.Errors {
			sb.WriteString("  - " + e + "\n")
		}
	}
	fmt.Fprintf(&sb, "%d inodes (%d dirs, %d files, %d symlinks, %d other), %d data blocks in use, %d orphans",
		r.Stats.Inodes, r.Stats.Dirs, r.Stats.Files, r.Stats.Symlinks, r.Stats.Specials,
		r.Stats.BlocksUsed, r.Stats.Orphans)
	if r.Stats.Snapshots > 0 {
		fmt.Fprintf(&sb, "; %d snapshot(s), %d COW records, %d snapshot-owned blocks",
			r.Stats.Snapshots, r.Stats.CowRecords, r.Stats.SnapBlocks)
	}
	return sb.String()
}

// Options tunes the checker.
type Options struct {
	// ReplayJournal performs journal recovery first (this writes to dev).
	// Without it, a non-empty journal is reported as an error.
	ReplayJournal bool
	// MaxErrors stops collecting after this many findings (0: 200).
	MaxErrors int
}

type checker struct {
	dev  blockdev.Device
	sb   *format.Superblock
	rep  *Report
	max  int
	full bool

	ibmap []byte
	bbmap []byte

	owner   map[uint32]uint32 // block -> owning inode
	inodes  map[uint32]*info
	refs    map[uint32]uint32 // inode -> references from directory entries
	subdirs map[uint32]uint32 // dir inode -> number of child directories (via ..)
	parent  map[uint32]uint32 // dir inode -> parent per its ".." entry
	dirOf   map[uint32]uint32 // dir inode -> directory holding its entry

	view bool // checking a snapshot view: inodes are found by scanning the table
}

type info struct {
	ino uint32
	in  format.Inode
}

func (c *checker) errf(f string, a ...any) {
	if len(c.rep.Errors) < c.max {
		c.rep.Errors = append(c.rep.Errors, fmt.Sprintf(f, a...))
	} else {
		c.full = true
	}
}

func (c *checker) readBlock(blk uint64) ([]byte, error) {
	b := make([]byte, format.BlockSize)
	_, err := c.dev.ReadAt(b, int64(blk)*format.BlockSize)
	return b, err
}

// Check verifies the image on dev.
func Check(dev blockdev.Device, o Options) (*Report, error) {
	c := &checker{
		dev: dev, rep: &Report{}, max: o.MaxErrors,
		owner: map[uint32]uint32{}, inodes: map[uint32]*info{},
		refs: map[uint32]uint32{}, subdirs: map[uint32]uint32{},
		parent: map[uint32]uint32{}, dirOf: map[uint32]uint32{},
	}
	if c.max == 0 {
		c.max = 200
	}
	b0, err := c.readBlock(0)
	if err != nil {
		return nil, err
	}
	if c.sb, err = format.UnmarshalSuperblock(b0); err != nil {
		c.errf("superblock: %v", err)
		return c.rep, nil
	}
	if dev.Size() < int64(c.sb.TotalBlocks)*format.BlockSize {
		c.errf("device smaller than filesystem")
		return c.rep, nil
	}
	if !c.checkJournal(o) {
		return c.rep, nil
	}
	if b0, err = c.readBlock(0); err != nil { // replay may have changed it
		return nil, err
	}
	if c.sb, err = format.UnmarshalSuperblock(b0); err != nil {
		c.errf("superblock after recovery: %v", err)
		return c.rep, nil
	}
	if err := c.loadBitmaps(); err != nil {
		return nil, err
	}
	if err := c.scanInodes(); err != nil {
		return nil, err
	}
	c.checkDirs()
	c.checkLinks()
	c.checkOrphans()
	c.checkSnapshots()
	c.checkBitmaps()
	if c.full {
		c.rep.Errors = append(c.rep.Errors, "... too many errors, output truncated")
	}
	return c.rep, nil
}

func (c *checker) checkJournal(o Options) bool {
	j, err := format.ReadJournal(c.dev, c.sb)
	if err != nil {
		c.errf("journal: %v", err)
		return false
	}
	txns, _, err := j.Scan()
	if err != nil {
		c.errf("journal scan: %v", err)
		return false
	}
	c.rep.Stats.JournalTxns = len(txns)
	if len(txns) > 0 {
		if !o.ReplayJournal {
			c.errf("journal holds %d committed transaction(s) that must be replayed first", len(txns))
			return false
		}
		if _, err := j.Recover(); err != nil {
			c.errf("journal recovery failed: %v", err)
			return false
		}
	}
	return true
}

func (c *checker) loadBitmaps() error {
	sb := c.sb
	c.ibmap = make([]byte, sb.InodeBitmapBlocks*format.BlockSize)
	c.bbmap = make([]byte, sb.BlockBitmapBlocks*format.BlockSize)
	for i := uint64(0); i < sb.InodeBitmapBlocks; i++ {
		b, err := c.readBlock(sb.InodeBitmapStart + i)
		if err != nil {
			return err
		}
		copy(c.ibmap[i*format.BlockSize:], b)
	}
	for i := uint64(0); i < sb.BlockBitmapBlocks; i++ {
		b, err := c.readBlock(sb.BlockBitmapStart + i)
		if err != nil {
			return err
		}
		copy(c.bbmap[i*format.BlockSize:], b)
	}
	return nil
}

// claim records that blk is owned by ino, reporting double ownership.
func (c *checker) claim(blk uint32, ino uint32, what string) bool {
	if uint64(blk) < c.sb.DataStart || uint64(blk) >= c.sb.TotalBlocks {
		c.errf("inode %d: %s pointer %d outside data area [%d,%d)", ino, what, blk, c.sb.DataStart, c.sb.TotalBlocks)
		return false
	}
	if prev, dup := c.owner[blk]; dup {
		c.errf("block %d claimed by inode %d (%s) and inode %d", blk, ino, what, prev)
		return false
	}
	c.owner[blk] = ino
	return true
}

func (c *checker) scanInodes() error {
	sb := c.sb
	// Slot 0 must be marked used, and inode 1 must exist as a directory.
	if !c.view && !format.BitTest(c.ibmap, 0) {
		c.errf("inode bitmap: reserved inode 0 not marked used")
	}
	var tbl []byte
	var tblBlk uint64 = ^uint64(0)
	for ino := uint32(1); ino < sb.InodeCount; ino++ {
		// A snapshot view has no inode bitmap of its own (the bitmap is never
		// preserved): an inode is in use iff its slot holds a non-zero mode.
		if !c.view && !format.BitTest(c.ibmap, ino) {
			continue
		}
		blk := sb.InodeTableStart + uint64(ino/format.InodesPerBlock)
		if blk != tblBlk {
			var err error
			if tbl, err = c.readBlock(blk); err != nil {
				return err
			}
			tblBlk = blk
		}
		off := int(ino%format.InodesPerBlock) * format.InodeSize
		in, err := format.DecodeInode(tbl[off:off+format.InodeSize], ino)
		if err != nil {
			c.errf("inode %d: %v", ino, err)
			continue
		}
		if in.Mode == 0 {
			if !c.view {
				c.errf("inode %d: marked allocated but has no mode", ino)
			}
			continue
		}
		c.inodes[ino] = &info{ino: ino, in: in}
		c.rep.Stats.Inodes++
		c.checkInode(ino, &in)
	}
	root := c.inodes[format.RootIno]
	if root == nil || !root.in.IsDir() {
		c.errf("root inode missing or not a directory")
	}
	return nil
}

func (c *checker) checkInode(ino uint32, in *format.Inode) {
	switch in.Mode & format.SIFMT {
	case format.SIFDIR:
		c.rep.Stats.Dirs++
	case format.SIFREG:
		c.rep.Stats.Files++
	case format.SIFLNK:
		c.rep.Stats.Symlinks++
	case format.SIFCHR, format.SIFBLK, format.SIFIFO, format.SIFSOCK:
		c.rep.Stats.Specials++
	default:
		c.errf("inode %d: invalid file type in mode %o", ino, in.Mode)
		return
	}
	if in.Mode&^uint32(format.SIFMT|0o7777) != 0 {
		c.errf("inode %d: stray mode bits %o", ino, in.Mode)
	}
	if in.Nlink > format.MaxLinks+2 {
		c.errf("inode %d: link count %d too large", ino, in.Nlink)
	}
	if in.InlineSymlink() {
		if uint64(len(in.Inline)) != in.Size {
			c.errf("inode %d: inline symlink size mismatch", ino)
		}
		return
	}
	counted := c.walkMap(ino, in)
	if counted != in.Blocks {
		c.errf("inode %d: i_blocks=%d but %d blocks are mapped", ino, in.Blocks, counted)
	}
	c.rep.Stats.BlocksUsed += counted
	switch {
	case in.IsDir():
		if in.Size%format.BlockSize != 0 || in.Size == 0 {
			c.errf("dir %d: size %d not a positive multiple of the block size", ino, in.Size)
		}
	case in.IsLnk():
		if in.Size == 0 || in.Size >= format.BlockSize || in.Blocks != 1 {
			c.errf("symlink %d: size %d with %d blocks", ino, in.Size, in.Blocks)
		}
	case in.IsReg():
		if in.Size > (uint64(format.NDirect)+1<<10+1<<20+1<<30)*format.BlockSize {
			c.errf("file %d: size %d exceeds the maximum", ino, in.Size)
		}
	default:
		if in.Blocks != 0 {
			c.errf("special inode %d owns %d blocks", ino, in.Blocks)
		}
	}
}

// walkMap claims every block of the inode's map and returns how many.
func (c *checker) walkMap(ino uint32, in *format.Inode) uint64 {
	n := uint64(0)
	for i, p := range in.Direct {
		if p != 0 && c.claim(p, ino, fmt.Sprintf("direct[%d]", i)) {
			n++
		}
	}
	for lvl, p := range [3]uint32{in.Ind, in.DInd, in.TInd} {
		if p != 0 {
			n += c.walkIndirect(ino, p, lvl+1)
		}
	}
	return n
}

func (c *checker) walkIndirect(ino uint32, p uint32, level int) uint64 {
	if !c.claim(p, ino, fmt.Sprintf("indirect(level %d)", level)) {
		return 0
	}
	n := uint64(1)
	b, err := c.readBlock(uint64(p))
	if err != nil {
		c.errf("inode %d: cannot read indirect block %d: %v", ino, p, err)
		return n
	}
	for i := 0; i < format.PtrsPerBlock; i++ {
		q := le32(b[i*4:])
		if q == 0 {
			continue
		}
		if level == 1 {
			if c.claim(q, ino, "data") {
				n++
			}
		} else {
			n += c.walkIndirect(ino, q, level-1)
		}
	}
	return n
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// dirBlocks returns the physical blocks of a directory in file order.
func (c *checker) dirBlocks(ino uint32, in *format.Inode) []uint32 {
	n := int(in.Size / format.BlockSize)
	out := make([]uint32, 0, n)
	var collect func(p uint32, level int)
	collect = func(p uint32, level int) {
		if p == 0 || uint64(p) < c.sb.DataStart || uint64(p) >= c.sb.TotalBlocks {
			return
		}
		b, err := c.readBlock(uint64(p))
		if err != nil {
			return
		}
		for i := 0; i < format.PtrsPerBlock; i++ {
			q := le32(b[i*4:])
			if level == 1 {
				out = append(out, q)
			} else if q != 0 {
				collect(q, level-1)
			} else {
				// A hole in the indirect tree: pad so file order is preserved.
				for k := 0; k < pow(format.PtrsPerBlock, level-1); k++ {
					out = append(out, 0)
				}
			}
		}
	}
	for _, p := range in.Direct {
		out = append(out, p)
	}
	if in.Ind != 0 || len(out) < n {
		collect(in.Ind, 1)
	}
	if len(out) < n {
		collect(in.DInd, 2)
	}
	if len(out) < n {
		collect(in.TInd, 3)
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func pow(b, e int) int {
	r := 1
	for ; e > 0; e-- {
		r *= b
	}
	return r
}

func (c *checker) checkDirs() {
	inos := make([]uint32, 0, len(c.inodes))
	for ino, inf := range c.inodes {
		if inf.in.IsDir() {
			inos = append(inos, ino)
		}
	}
	slices.Sort(inos)
	for _, dino := range inos {
		c.checkDir(dino, &c.inodes[dino].in)
	}
	// Reachability and cycle check: every directory must reach the root
	// through its recorded parent chain.
	for _, dino := range inos {
		cur, steps := dino, 0
		for cur != format.RootIno {
			p, ok := c.dirOf[cur]
			if !ok {
				if c.inodes[cur].in.Nlink != 0 {
					c.errf("dir %d is not reachable from the root", dino)
				}
				break
			}
			cur = p
			if steps++; steps > len(inos) {
				c.errf("dir %d is part of a directory cycle", dino)
				break
			}
		}
	}
}

func (c *checker) checkDir(dino uint32, din *format.Inode) {
	blocks := c.dirBlocks(dino, din)
	if uint64(len(blocks)) != din.Size/format.BlockSize {
		c.errf("dir %d: size says %d blocks, map has %d", dino, din.Size/format.BlockSize, len(blocks))
	}
	seen := map[string]bool{}
	nEnt := 0
	for bi, p := range blocks {
		if p == 0 {
			c.errf("dir %d: hole at block %d", dino, bi)
			continue
		}
		b, err := c.readBlock(uint64(p))
		if err != nil {
			c.errf("dir %d: read block %d: %v", dino, p, err)
			continue
		}
		off := 0
		for off < format.BlockSize {
			d, err := format.ParseDirent(b, off)
			if err != nil {
				c.errf("dir %d block %d: %v", dino, bi, err)
				break
			}
			if d.Ino != 0 {
				if format.DirentLen(len(d.Name)) > int(d.RecLen) {
					c.errf("dir %d: entry %q does not fit its rec_len", dino, d.Name)
				}
				c.checkEntry(dino, bi, off, nEnt, &d, seen)
				nEnt++
			}
			off += int(d.RecLen)
		}
		if off != format.BlockSize {
			c.errf("dir %d block %d: entries do not tile the block (%d)", dino, bi, off)
		}
	}
	if !seen["."] || !seen[".."] {
		c.errf("dir %d: missing '.' or '..'", dino)
	}
}

func (c *checker) checkEntry(dino uint32, bi, off, idx int, d *format.Dirent, seen map[string]bool) {
	name := d.Name
	if name == "" || strings.ContainsAny(name, "/\x00") {
		c.errf("dir %d: illegal name %q", dino, name)
	}
	if seen[name] {
		c.errf("dir %d: duplicate name %q", dino, name)
	}
	seen[name] = true
	if bi == 0 && off == 0 && name != "." {
		c.errf("dir %d: first entry is %q, not '.'", dino, name)
	}
	target := c.inodes[d.Ino]
	if target == nil {
		c.errf("dir %d: entry %q -> inode %d which is not allocated", dino, name, d.Ino)
		return
	}
	if want := format.FTypeOf(target.in.Mode); want != d.FType {
		c.errf("dir %d: entry %q has type %d but inode %d is type %d", dino, name, d.FType, d.Ino, want)
	}
	switch name {
	case ".":
		if d.Ino != dino {
			c.errf("dir %d: '.' points to %d", dino, d.Ino)
		}
		c.refs[d.Ino]++
	case "..":
		c.parent[dino] = d.Ino
		if !target.in.IsDir() {
			c.errf("dir %d: '..' is not a directory", dino)
		}
		// A removed-but-still-open directory keeps its stale '..' entry; it
		// no longer contributes to its former parent's link count.
		if c.inodes[dino].in.Nlink != 0 {
			c.subdirs[d.Ino]++
			c.refs[d.Ino]++
		}
	default:
		c.refs[d.Ino]++
		if target.in.IsDir() {
			if prev, dup := c.dirOf[d.Ino]; dup {
				c.errf("directory %d has two parents (%d and %d)", d.Ino, prev, dino)
			}
			c.dirOf[d.Ino] = dino
		}
	}
}

func (c *checker) checkLinks() {
	for ino, inf := range c.inodes {
		in := &inf.in
		refs := c.refs[ino]
		if in.IsDir() {
			// A live directory is referenced by its parent entry, its own
			// '.', and one '..' per child directory. The root has no parent
			// entry but its '..' points to itself.
			want := refs
			if in.Nlink == 0 {
				continue // removed but open: handled by the orphan check
			}
			if ino == format.RootIno {
				if p := c.parent[ino]; p != format.RootIno {
					c.errf("root '..' points to %d", p)
				}
			} else if _, ok := c.dirOf[ino]; ok {
				if p := c.parent[ino]; p != c.dirOf[ino] {
					c.errf("dir %d: '..' is %d but it lives in %d", ino, p, c.dirOf[ino])
				}
			}
			if in.Nlink != want {
				c.errf("dir %d: link count %d, found %d references", ino, in.Nlink, want)
			}
			continue
		}
		if in.Nlink != refs {
			c.errf("inode %d: link count %d, found %d directory entries", ino, in.Nlink, refs)
		}
	}
}

func (c *checker) checkOrphans() {
	onList := map[uint32]bool{}
	for p, steps := c.sb.OrphanHead, 0; p != 0; steps++ {
		if steps > int(c.sb.InodeCount) {
			c.errf("orphan list has a cycle")
			break
		}
		inf := c.inodes[p]
		if inf == nil {
			c.errf("orphan list entry %d is not an allocated inode", p)
			break
		}
		if inf.in.Nlink != 0 {
			c.errf("orphan list entry %d has link count %d", p, inf.in.Nlink)
		}
		if onList[p] {
			c.errf("orphan list revisits inode %d", p)
			break
		}
		onList[p] = true
		p = inf.in.OrphanNext
	}
	c.rep.Stats.Orphans = len(onList)
	for ino, inf := range c.inodes {
		if inf.in.Nlink == 0 && !onList[ino] {
			c.errf("inode %d has no links and is not on the orphan list (leaked)", ino)
		}
	}
}

func (c *checker) checkBitmaps() {
	sb := c.sb
	for blk := uint64(0); blk < sb.BlockBitmapBlocks*format.BitsPerBlock; blk++ {
		set := format.BitTest(c.bbmap, uint32(blk))
		switch {
		case blk >= sb.TotalBlocks:
			if !set {
				c.errf("block bitmap: padding bit %d is clear", blk)
				return
			}
		case blk < sb.DataStart:
			c.rep.Stats.MetaBlocks++
			if !set {
				c.errf("block bitmap: metadata block %d not marked used", blk)
			}
		default:
			_, owned := c.owner[uint32(blk)]
			if owned && !set {
				c.errf("block bitmap: block %d is in use by inode %d but marked free", blk, c.owner[uint32(blk)])
			} else if !owned && set {
				c.errf("block bitmap: block %d is marked used but owned by nothing (leak)", blk)
			}
		}
		if len(c.rep.Errors) >= c.max {
			c.full = true
			return
		}
	}
	for ino := uint64(sb.InodeCount); ino < sb.IBits(); ino++ {
		if !format.BitTest(c.ibmap, uint32(ino)) {
			c.errf("inode bitmap: padding bit %d is clear", ino)
			return
		}
	}
}
