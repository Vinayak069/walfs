package engine

import (
	"syscall"

	"github.com/vinayak-singh/walfs/internal/format"
)

// ---------------------------------------------------------------------------
// Directories
//
// On disk a directory is an ordinary file whose blocks hold ext2-style
// variable-length entries chained by rec_len (a linear structure). To avoid
// O(n) lookups, each directory touched since mount gets an in-memory index
// built once by scanning its blocks:
//
//	name -> (inode, type, block index)       O(1) lookup / existence checks
//	per-block largest free slot              O(#blocks) insert placement
//
// The index is purely derived state: it is rebuilt after a remount and never
// trusted over the disk (fsck ignores it).

type dent struct {
	ino uint32
	ft  uint8
	blk uint32 // index of the directory block holding the entry
}

type dirIndex struct {
	ents  map[string]dent
	slack []uint16 // per directory block: largest record that fits in free space
}

const maxCachedDirEntries = 1 << 20

// blockSlack validates the entry chain of a directory block and returns the
// largest record that could be inserted into it.
func blockSlack(data []byte, visit func(off int, d format.Dirent)) (int, error) {
	off, best := 0, 0
	for off < format.BlockSize {
		d, err := format.ParseDirent(data, off)
		if err != nil {
			return 0, err
		}
		used := 0
		if d.Ino != 0 {
			used = format.DirentLen(len(d.Name))
			if used > int(d.RecLen) {
				return 0, corruptf("dirent at %d: name does not fit rec_len", off)
			}
			if visit != nil {
				visit(off, d)
			}
		}
		if s := int(d.RecLen) - used; s > best {
			best = s
		}
		off += int(d.RecLen)
	}
	if off != format.BlockSize {
		return 0, corruptf("dirent chain overruns block")
	}
	return best, nil
}

// dirBlockBuf returns directory block bi for reading.
func (fs *FS) dirBlockBuf(din *format.Inode, bi uint32) (*buf, error) {
	pb, err := fs.newMapper(din).lookup(uint64(bi))
	if err != nil {
		return nil, err
	}
	if pb == 0 {
		return nil, corruptf("hole in directory at block %d", bi)
	}
	b, err := fs.c.get(uint64(pb))
	if err != nil {
		return nil, syscall.EIO
	}
	return b, nil
}

// dirBlockForWrite is dirBlockBuf but joins the transaction.
func (fs *FS) dirBlockForWrite(din *format.Inode, bi uint32) (*buf, error) {
	pb, err := fs.newMapper(din).lookup(uint64(bi))
	if err != nil {
		return nil, err
	}
	if pb == 0 {
		return nil, corruptf("hole in directory at block %d", bi)
	}
	b, err := fs.metaBuf(uint64(pb))
	if err != nil {
		return nil, syscall.EIO
	}
	return b, nil
}

// dirIdx returns the (lazily built) index for directory dino.
func (fs *FS) dirIdx(dino uint32, din *format.Inode) (*dirIndex, error) {
	fs.dirMu.Lock()
	defer fs.dirMu.Unlock()
	if ix, ok := fs.dirs[dino]; ok {
		return ix, nil
	}
	n := int(din.Size / format.BlockSize)
	ix := &dirIndex{ents: make(map[string]dent), slack: make([]uint16, n)}
	for bi := 0; bi < n; bi++ {
		b, err := fs.dirBlockBuf(din, uint32(bi))
		if err != nil {
			return nil, err
		}
		s, err := blockSlack(b.data, func(off int, d format.Dirent) {
			if _, dup := ix.ents[d.Name]; !dup {
				ix.ents[d.Name] = dent{ino: d.Ino, ft: d.FType, blk: uint32(bi)}
			}
		})
		if err != nil {
			return nil, err
		}
		ix.slack[bi] = uint16(min(s, 0xffff))
	}
	if fs.dirEnts+len(ix.ents) > maxCachedDirEntries {
		clear(fs.dirs)
		fs.dirEnts = 0
	}
	fs.dirs[dino] = ix
	fs.dirEnts += len(ix.ents)
	return ix, nil
}

func (fs *FS) dropDirIdx(dino uint32) {
	fs.dirMu.Lock()
	if ix, ok := fs.dirs[dino]; ok {
		fs.dirEnts -= len(ix.ents)
		delete(fs.dirs, dino)
	}
	fs.dirMu.Unlock()
}

// dirLookup finds name in directory dino.
func (fs *FS) dirLookup(dino uint32, din *format.Inode, name string) (dent, bool, error) {
	ix, err := fs.dirIdx(dino, din)
	if err != nil {
		return dent{}, false, err
	}
	d, ok := ix.ents[name]
	return d, ok, nil
}

func (fs *FS) dirIsEmpty(dino uint32, din *format.Inode) (bool, error) {
	ix, err := fs.dirIdx(dino, din)
	if err != nil {
		return false, err
	}
	return len(ix.ents) <= 2, nil
}

// insertInBlock places a new entry into the first record with enough slack.
func insertInBlock(data []byte, name string, ino uint32, ft uint8) bool {
	need := format.DirentLen(len(name))
	for off := 0; off < format.BlockSize; {
		d, err := format.ParseDirent(data, off)
		if err != nil {
			return false
		}
		rl := int(d.RecLen)
		if d.Ino == 0 {
			if rl >= need {
				format.PutDirent(data, off, ino, rl, ft, name)
				return true
			}
		} else if used := format.DirentLen(len(d.Name)); rl-used >= need {
			format.SetRecLen(data, off, used)
			format.PutDirent(data, off+used, ino, rl-used, ft, name)
			return true
		}
		off += rl
	}
	return false
}

// dirInsert adds name -> ino. The caller has checked that name is absent.
// din may be modified (size/blocks) and must be persisted by the caller.
func (fs *FS) dirInsert(dino uint32, din *format.Inode, name string, ino uint32, ft uint8) error {
	ix, err := fs.dirIdx(dino, din)
	if err != nil {
		return err
	}
	need := format.DirentLen(len(name))
	for bi, s := range ix.slack {
		if int(s) < need {
			continue
		}
		b, err := fs.dirBlockForWrite(din, uint32(bi))
		if err != nil {
			return err
		}
		if !insertInBlock(b.data, name, ino, ft) {
			return corruptf("directory %d block %d slack mismatch", dino, bi)
		}
		return fs.dirCommitInsert(ix, b, bi, name, ino, ft)
	}
	// Grow the directory by one block.
	fb := din.Size / format.BlockSize
	goal := uint64(0)
	if fb > 0 {
		if prev, _ := fs.newMapper(din).lookup(fb - 1); prev != 0 {
			goal = uint64(prev) + 1
		}
	}
	pb, _, err := fs.bmapAllocMeta(din, fb, goal)
	if err != nil {
		return err
	}
	b := fs.newMeta(uint64(pb))
	format.PutDirent(b.data, 0, 0, format.BlockSize, 0, "")
	din.Size += format.BlockSize
	ix.slack = append(ix.slack, format.BlockSize)
	bi := len(ix.slack) - 1
	if !insertInBlock(b.data, name, ino, ft) {
		return corruptf("fresh directory block rejected entry")
	}
	return fs.dirCommitInsert(ix, b, bi, name, ino, ft)
}

func (fs *FS) dirCommitInsert(ix *dirIndex, b *buf, bi int, name string, ino uint32, ft uint8) error {
	s, err := blockSlack(b.data, nil)
	if err != nil {
		return err
	}
	ix.slack[bi] = uint16(min(s, 0xffff))
	ix.ents[name] = dent{ino: ino, ft: ft, blk: uint32(bi)}
	fs.dirMu.Lock()
	fs.dirEnts++
	fs.dirMu.Unlock()
	return nil
}

// dirRemove deletes name from the directory and returns what it pointed to.
func (fs *FS) dirRemove(dino uint32, din *format.Inode, name string) (dent, error) {
	ix, err := fs.dirIdx(dino, din)
	if err != nil {
		return dent{}, err
	}
	e, ok := ix.ents[name]
	if !ok {
		return dent{}, syscall.ENOENT
	}
	b, err := fs.dirBlockForWrite(din, e.blk)
	if err != nil {
		return dent{}, err
	}
	prev := -1
	found := false
	for off := 0; off < format.BlockSize; {
		d, perr := format.ParseDirent(b.data, off)
		if perr != nil {
			return dent{}, corruptf("directory %d: %v", dino, perr)
		}
		if d.Ino != 0 && d.Name == name {
			if prev >= 0 {
				pd, _ := format.ParseDirent(b.data, prev)
				format.SetRecLen(b.data, prev, int(pd.RecLen)+int(d.RecLen))
			} else {
				format.ClearIno(b.data, off)
			}
			found = true
			break
		}
		prev = off
		off += int(d.RecLen)
	}
	if !found {
		return dent{}, corruptf("directory %d: indexed name %q missing on disk", dino, name)
	}
	s, err := blockSlack(b.data, nil)
	if err != nil {
		return dent{}, err
	}
	ix.slack[e.blk] = uint16(min(s, 0xffff))
	delete(ix.ents, name)
	fs.dirMu.Lock()
	fs.dirEnts--
	fs.dirMu.Unlock()
	return e, nil
}

// dirSetIno retargets an existing entry (rename-over, ".." fix-up).
func (fs *FS) dirSetIno(dino uint32, din *format.Inode, name string, ino uint32, ft uint8) error {
	ix, err := fs.dirIdx(dino, din)
	if err != nil {
		return err
	}
	e, ok := ix.ents[name]
	if !ok {
		return syscall.ENOENT
	}
	b, err := fs.dirBlockForWrite(din, e.blk)
	if err != nil {
		return err
	}
	for off := 0; off < format.BlockSize; {
		d, perr := format.ParseDirent(b.data, off)
		if perr != nil {
			return corruptf("directory %d: %v", dino, perr)
		}
		if d.Ino != 0 && d.Name == name {
			le.PutUint32(b.data[off:], ino)
			b.data[off+7] = ft
			ix.ents[name] = dent{ino: ino, ft: ft, blk: e.blk}
			return nil
		}
		off += int(d.RecLen)
	}
	return corruptf("directory %d: indexed name %q missing on disk", dino, name)
}

// DirEntry is one directory listing record.
type DirEntry struct {
	Name string
	Ino  uint64
	Type uint8  // format.FT*
	Next uint64 // cookie that resumes after this entry
}

// readDirEntries lists entries starting at the given byte-offset cookie.
func (fs *FS) readDirEntries(din *format.Inode, cookie uint64, fn func(DirEntry) bool) error {
	nblocks := din.Size / format.BlockSize
	for bi := cookie / format.BlockSize; bi < nblocks; bi++ {
		b, err := fs.dirBlockBuf(din, uint32(bi))
		if err != nil {
			return err
		}
		for off := 0; off < format.BlockSize; {
			d, perr := format.ParseDirent(b.data, off)
			if perr != nil {
				return corruptf("directory block %d: %v", bi, perr)
			}
			abs := bi*format.BlockSize + uint64(off)
			next := abs + uint64(d.RecLen)
			off += int(d.RecLen)
			if d.Ino == 0 || abs < cookie {
				continue
			}
			if !fn(DirEntry{Name: d.Name, Ino: uint64(d.Ino), Type: d.FType, Next: next}) {
				return nil
			}
		}
	}
	return nil
}
