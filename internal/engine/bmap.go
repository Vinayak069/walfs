package engine

import (
	"encoding/binary"
	"syscall"

	"github.com/Vinayak069/walfs/internal/format"
)

// ---------------------------------------------------------------------------
// File block mapping: 12 direct pointers, then single, double and triple
// indirect trees (1024 pointers per 4 KiB block). Pointer value 0 means
// "hole". Maximum file size is about 4 TiB.

const (
	ptrs = format.PtrsPerBlock
	// MaxFileBlocks is the number of file blocks addressable by the map.
	MaxFileBlocks = uint64(format.NDirect) + ptrs + ptrs*ptrs + ptrs*ptrs*ptrs
	// MaxFileSize is the largest representable file.
	MaxFileSize = MaxFileBlocks * format.BlockSize
)

var le = binary.LittleEndian

// blockPath decomposes a file block number into the indirection depth (0 for
// direct) and the index to follow at each level.
func blockPath(fb uint64) (depth int, idx [3]uint32, ok bool) {
	if fb < format.NDirect {
		return 0, [3]uint32{uint32(fb)}, true
	}
	fb -= format.NDirect
	if fb < ptrs {
		return 1, [3]uint32{uint32(fb)}, true
	}
	fb -= ptrs
	if fb < ptrs*ptrs {
		return 2, [3]uint32{uint32(fb / ptrs), uint32(fb % ptrs)}, true
	}
	fb -= ptrs * ptrs
	if fb < ptrs*ptrs*ptrs {
		return 3, [3]uint32{uint32(fb / (ptrs * ptrs)), uint32(fb / ptrs % ptrs), uint32(fb % ptrs)}, true
	}
	return 0, idx, false
}

func rootPtr(in *format.Inode, depth int) *uint32 {
	switch depth {
	case 1:
		return &in.Ind
	case 2:
		return &in.DInd
	default:
		return &in.TInd
	}
}

func (fs *FS) validBlk(p uint32) error {
	if uint64(p) < fs.sb.DataStart || uint64(p) >= fs.sb.TotalBlocks {
		return corruptf("block pointer %d out of range", p)
	}
	return nil
}

// mapper resolves file blocks to device blocks without allocating, remembering
// the last leaf indirect block so sequential access costs one cache hit per
// 1024 blocks.
type mapper struct {
	fs      *FS
	in      *format.Inode
	leafBlk uint32
	leaf    *buf
}

func (fs *FS) newMapper(in *format.Inode) *mapper { return &mapper{fs: fs, in: in} }

// lookup returns the device block holding file block fb, or 0 for a hole.
func (m *mapper) lookup(fb uint64) (uint32, error) {
	depth, idx, ok := blockPath(fb)
	if !ok {
		return 0, syscall.EFBIG
	}
	if depth == 0 {
		return m.in.Direct[idx[0]], nil
	}
	ptr := *rootPtr(m.in, depth)
	for lvl := 0; lvl < depth; lvl++ {
		if ptr == 0 {
			return 0, nil
		}
		if err := m.fs.validBlk(ptr); err != nil {
			return 0, err
		}
		var b *buf
		if lvl == depth-1 && m.leaf != nil && m.leafBlk == ptr {
			b = m.leaf
		} else {
			var err error
			if b, err = m.fs.c.get(uint64(ptr)); err != nil {
				return 0, syscall.EIO
			}
			if lvl == depth-1 {
				m.leaf, m.leafBlk = b, ptr
			}
		}
		ptr = le.Uint32(b.data[idx[lvl]*4:])
	}
	if ptr != 0 {
		if err := m.fs.validBlk(ptr); err != nil {
			return 0, err
		}
	}
	return ptr, nil
}

// bmapAlloc returns the device block for file block fb, allocating it (and any
// missing indirect blocks) when absent. The new data block is returned
// unwritten; the caller initialises it. created reports allocation.
//
// On error, any indirect blocks allocated so far stay linked and accounted in
// in.Blocks, so the caller must still persist the inode.
func (fs *FS) bmapAlloc(in *format.Inode, fb uint64, goal uint64) (pb uint32, created bool, err error) {
	return fs.bmapAllocX(in, fb, goal, false)
}

// bmapAllocMeta is bmapAlloc for blocks that are themselves metadata
// (directory and symlink blocks): they may use the reserved pool.
func (fs *FS) bmapAllocMeta(in *format.Inode, fb uint64, goal uint64) (pb uint32, created bool, err error) {
	return fs.bmapAllocX(in, fb, goal, true)
}

func (fs *FS) bmapAllocX(in *format.Inode, fb uint64, goal uint64, metaData bool) (pb uint32, created bool, err error) {
	depth, idx, ok := blockPath(fb)
	if !ok {
		return 0, false, syscall.EFBIG
	}
	if depth == 0 {
		if p := in.Direct[idx[0]]; p != 0 {
			if err := fs.validBlk(p); err != nil {
				return 0, false, err
			}
			return p, false, nil
		}
		nb, err := fs.allocBlock(goal, metaData)
		if err != nil {
			return 0, false, err
		}
		in.Direct[idx[0]] = nb
		in.Blocks++
		return nb, true, nil
	}
	root := rootPtr(in, depth)
	if *root == 0 {
		nb, err := fs.allocBlock(goal, true)
		if err != nil {
			return 0, false, err
		}
		fs.newMeta(uint64(nb))
		*root = nb
		in.Blocks++
	}
	ptr := *root
	for lvl := 0; lvl < depth; lvl++ {
		if err := fs.validBlk(ptr); err != nil {
			return 0, false, err
		}
		last := lvl == depth-1
		b, err := fs.c.get(uint64(ptr))
		if err != nil {
			return 0, false, syscall.EIO
		}
		next := le.Uint32(b.data[idx[lvl]*4:])
		if next == 0 {
			nb, aerr := fs.allocBlock(goal, !last || metaData)
			if aerr != nil {
				return 0, false, aerr
			}
			if b, err = fs.metaBuf(uint64(ptr)); err != nil {
				return 0, false, syscall.EIO
			}
			le.PutUint32(b.data[idx[lvl]*4:], nb)
			in.Blocks++
			if !last {
				fs.newMeta(uint64(nb))
			} else {
				created = true
			}
			next = nb
		}
		ptr = next
	}
	if err := fs.validBlk(ptr); err != nil {
		return 0, false, err
	}
	return ptr, created, nil
}

// freeState tracks progress of a block-freeing pass.
type freeState struct {
	budget int    // data+indirect blocks we may still free in this pass
	low    uint64 // every mapped file block >= low has been freed
	done   bool   // false when the budget ran out
	// leaf is how the blocks at the leaves of the map were used. Directory
	// and symlink blocks are journaled metadata and must be revoked when
	// freed, otherwise replaying their stale journal image after a crash
	// would clobber the file data they are later reused for.
	leaf blockClass
}

// leafClassOf returns the class of an inode's leaf blocks.
func leafClassOf(in *format.Inode) blockClass {
	if in.IsReg() {
		return classData
	}
	return classMeta
}

func span(level int) uint64 {
	s := uint64(1)
	for i := 0; i < level; i++ {
		s *= ptrs
	}
	return s
}

// freeFrom frees every mapped block with file block number >= from, walking
// from the end of the file backwards so that an interrupted pass leaves a
// valid shorter file. At most st.budget blocks are freed per call.
func (fs *FS) freeFrom(in *format.Inode, from uint64, st *freeState) error {
	st.done = true
	st.low = ^uint64(0)
	// Highest first: triple, double, single indirect, then direct blocks.
	bases := [3]uint64{
		format.NDirect + ptrs + ptrs*ptrs, // triple
		format.NDirect + ptrs,             // double
		format.NDirect,                    // single
	}
	for i, lvl := range [3]int{3, 2, 1} {
		p := rootPtr(in, lvl)
		if *p == 0 {
			continue
		}
		base := bases[i]
		if base+span(lvl) <= from {
			continue
		}
		empty, err := fs.freeSubtree(in, *p, lvl, base, from, st)
		if err != nil {
			return err
		}
		if empty {
			*p = 0
		}
		if !st.done {
			return nil
		}
	}
	for i := format.NDirect - 1; i >= 0; i-- {
		if uint64(i) < from {
			break
		}
		if in.Direct[i] == 0 {
			continue
		}
		if st.budget <= 0 {
			st.done = false
			return nil
		}
		if err := fs.validBlk(in.Direct[i]); err != nil {
			return err
		}
		if err := fs.freeBlock(in.Direct[i], st.leaf); err != nil {
			return err
		}
		in.Direct[i] = 0
		in.Blocks--
		st.budget--
		st.low = uint64(i)
	}
	return nil
}

// freeSubtree frees the part of the indirect subtree rooted at block ptr
// (covering file blocks [base, base+span(level))) that lies at or beyond
// from. It reports whether the subtree became empty (and was freed itself).
func (fs *FS) freeSubtree(in *format.Inode, ptr uint32, level int, base, from uint64, st *freeState) (empty bool, err error) {
	if err := fs.validBlk(ptr); err != nil {
		return false, err
	}
	b, err := fs.metaBuf(uint64(ptr))
	if err != nil {
		return false, syscall.EIO
	}
	childSpan := span(level - 1)
	for i := ptrs - 1; i >= 0; i-- {
		cbase := base + uint64(i)*childSpan
		if cbase+childSpan <= from {
			break
		}
		child := le.Uint32(b.data[i*4:])
		if child == 0 {
			continue
		}
		if st.budget <= 0 {
			st.done = false
			return false, nil
		}
		if level == 1 {
			if err := fs.validBlk(child); err != nil {
				return false, err
			}
			if err := fs.freeBlock(child, st.leaf); err != nil {
				return false, err
			}
			le.PutUint32(b.data[i*4:], 0)
			in.Blocks--
			st.budget--
			st.low = cbase
			continue
		}
		cempty, err := fs.freeSubtree(in, child, level-1, cbase, from, st)
		if err != nil {
			return false, err
		}
		if cempty {
			le.PutUint32(b.data[i*4:], 0)
		}
		if !st.done {
			return false, nil
		}
		st.low = cbase
	}
	// Free this indirect block if nothing remains in it.
	for i := 0; i < ptrs; i++ {
		if le.Uint32(b.data[i*4:]) != 0 {
			return false, nil
		}
	}
	if err := fs.freeBlock(ptr, classMeta); err != nil {
		return false, err
	}
	in.Blocks--
	st.budget--
	return true, nil
}
