// Package model is a deliberately naive in-memory reference implementation of
// the walfs VFS semantics. It has no blocks, no journal and no on-disk
// layout: just maps and byte slices. Randomised tests run identical operation
// sequences against the model and the real engine and require identical
// results, and crash tests require every recovered image to equal a model
// state. The model is only trustworthy because it is simple enough to review
// by eye.
package model

import (
	"bytes"
	"sort"
	"syscall"

	"github.com/vinayak-singh/walfs/internal/engine"
	"github.com/vinayak-singh/walfs/internal/format"
)

type node struct {
	mode                uint32
	uid, gid            uint32
	nlink               uint32
	rdev                uint64
	atime, mtime, ctime int64
	data                []byte            // regular files
	target              []byte            // symlinks
	ents                map[string]uint64 // directories (including "." and "..")
}

// Model implements the same operations as engine.FS.
type Model struct {
	nodes map[uint64]*node
	next  uint64
	// Now supplies the timestamp of the current operation.
	Now func() int64
}

const (
	sISUID = 0o4000
	sISGID = 0o2000
	sIXGRP = 0o010
)

// New returns a model holding only an empty root directory (inode 1).
func New(now func() int64) *Model {
	m := &Model{nodes: map[uint64]*node{}, next: 2, Now: now}
	t := now()
	m.nodes[1] = &node{
		mode: format.SIFDIR | 0o755, nlink: 2,
		atime: t, mtime: t, ctime: t,
		ents: map[string]uint64{".": 1, "..": 1},
	}
	return m
}

func checkName(name string) error {
	switch {
	case name == "":
		return syscall.ENOENT
	case len(name) > format.NameMax:
		return syscall.ENAMETOOLONG
	}
	for i := 0; i < len(name); i++ {
		if name[i] == '/' || name[i] == 0 {
			return syscall.EINVAL
		}
	}
	return nil
}

func (m *Model) get(ino uint64) (*node, error) {
	n := m.nodes[ino]
	if n == nil {
		return nil, syscall.ESTALE
	}
	return n, nil
}

func (m *Model) dir(ino uint64) (*node, error) {
	n, err := m.get(ino)
	if err != nil {
		return nil, err
	}
	if n.mode&format.SIFMT != format.SIFDIR {
		return nil, syscall.ENOTDIR
	}
	return n, nil
}

func (m *Model) liveDir(ino uint64) (*node, error) {
	n, err := m.dir(ino)
	if err == nil && n.nlink == 0 {
		return n, syscall.ENOENT
	}
	return n, err
}

func attr(ino uint64, n *node) engine.Attr {
	size := uint64(len(n.data))
	switch n.mode & format.SIFMT {
	case format.SIFLNK:
		size = uint64(len(n.target))
	case format.SIFDIR:
		size = 0 // directory size is an implementation detail; not compared
	}
	return engine.Attr{
		Ino: ino, Mode: n.mode, Nlink: n.nlink, UID: n.uid, GID: n.gid,
		Size: size, Atime: n.atime, Mtime: n.mtime, Ctime: n.ctime, Rdev: n.rdev,
	}
}

func (m *Model) Lookup(parent uint64, name string) (engine.Attr, error) {
	if err := checkName(name); err != nil {
		return engine.Attr{}, err
	}
	p, err := m.dir(parent)
	if err != nil {
		return engine.Attr{}, err
	}
	ino, ok := p.ents[name]
	if !ok {
		return engine.Attr{}, syscall.ENOENT
	}
	return attr(ino, m.nodes[ino]), nil
}

func (m *Model) Getattr(ino uint64) (engine.Attr, error) {
	n, err := m.get(ino)
	if err != nil {
		return engine.Attr{}, err
	}
	return attr(ino, n), nil
}

func (m *Model) Readlink(ino uint64) ([]byte, error) {
	n, err := m.get(ino)
	if err != nil {
		return nil, err
	}
	if n.mode&format.SIFMT != format.SIFLNK {
		return nil, syscall.EINVAL
	}
	return append([]byte(nil), n.target...), nil
}

// Readdir returns all entries (order unspecified; callers sort).
func (m *Model) Readdir(ino uint64) ([]engine.DirEntry, error) {
	n, err := m.dir(ino)
	if err != nil {
		return nil, err
	}
	var out []engine.DirEntry
	for name, c := range n.ents {
		out = append(out, engine.DirEntry{Name: name, Ino: c, Type: format.FTypeOf(m.nodes[c].mode)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Model) Mknod(ctx engine.Ctx, parent uint64, name string, mode uint32, rdev uint64) (engine.Attr, error) {
	switch mode & format.SIFMT {
	case format.SIFREG, format.SIFIFO, format.SIFSOCK, format.SIFCHR, format.SIFBLK:
	case 0:
		mode |= format.SIFREG
	default:
		return engine.Attr{}, syscall.EINVAL
	}
	return m.create(ctx, parent, name, mode, rdev, nil)
}

func (m *Model) Mkdir(ctx engine.Ctx, parent uint64, name string, mode uint32) (engine.Attr, error) {
	return m.create(ctx, parent, name, format.SIFDIR|(mode&0o7777), 0, nil)
}

func (m *Model) Symlink(ctx engine.Ctx, parent uint64, name string, target []byte) (engine.Attr, error) {
	if len(target) == 0 {
		return engine.Attr{}, syscall.ENOENT
	}
	if len(target) >= format.BlockSize {
		return engine.Attr{}, syscall.ENAMETOOLONG
	}
	return m.create(ctx, parent, name, format.SIFLNK|0o777, 0, target)
}

func (m *Model) create(ctx engine.Ctx, parent uint64, name string, mode uint32, rdev uint64, target []byte) (engine.Attr, error) {
	if err := checkName(name); err != nil {
		return engine.Attr{}, err
	}
	p, err := m.liveDir(parent)
	if err != nil {
		return engine.Attr{}, err
	}
	if name == "." || name == ".." {
		return engine.Attr{}, syscall.EEXIST
	}
	if _, ok := p.ents[name]; ok {
		return engine.Attr{}, syscall.EEXIST
	}
	isDir := mode&format.SIFMT == format.SIFDIR
	if isDir && p.nlink >= format.MaxLinks {
		return engine.Attr{}, syscall.EMLINK
	}
	now := m.Now()
	gid := ctx.GID
	if p.mode&sISGID != 0 {
		gid = p.gid
		if isDir {
			mode |= sISGID
		}
	}
	n := &node{mode: mode, uid: ctx.UID, gid: gid, nlink: 1, rdev: rdev, atime: now, mtime: now, ctime: now}
	ino := m.next
	m.next++
	switch {
	case isDir:
		n.nlink = 2
		n.ents = map[string]uint64{".": ino, "..": parent}
		p.nlink++
	case target != nil:
		n.target = append([]byte(nil), target...)
	}
	m.nodes[ino] = n
	p.ents[name] = ino
	p.mtime, p.ctime = now, now
	return attr(ino, n), nil
}

func (m *Model) Link(ino uint64, newparent uint64, name string) (engine.Attr, error) {
	if err := checkName(name); err != nil {
		return engine.Attr{}, err
	}
	n, err := m.get(ino)
	if err != nil {
		return engine.Attr{}, err
	}
	if n.nlink == 0 {
		return engine.Attr{}, syscall.ENOENT
	}
	if n.mode&format.SIFMT == format.SIFDIR {
		return engine.Attr{}, syscall.EPERM
	}
	if n.nlink >= format.MaxLinks {
		return engine.Attr{}, syscall.EMLINK
	}
	p, err := m.liveDir(newparent)
	if err != nil {
		return engine.Attr{}, err
	}
	if name == "." || name == ".." {
		return engine.Attr{}, syscall.EEXIST
	}
	if _, ok := p.ents[name]; ok {
		return engine.Attr{}, syscall.EEXIST
	}
	now := m.Now()
	p.ents[name] = ino
	n.nlink++
	n.ctime = now
	p.mtime, p.ctime = now, now
	return attr(ino, n), nil
}

func (m *Model) Unlink(parent uint64, name string) error { return m.remove(parent, name, false) }
func (m *Model) Rmdir(parent uint64, name string) error  { return m.remove(parent, name, true) }

func (m *Model) remove(parent uint64, name string, wantDir bool) error {
	if err := checkName(name); err != nil {
		return err
	}
	p, err := m.dir(parent)
	if err != nil {
		return err
	}
	if wantDir && name == "." {
		return syscall.EINVAL
	}
	if name == "." || name == ".." {
		if wantDir {
			return syscall.ENOTEMPTY
		}
		return syscall.EISDIR
	}
	ino, ok := p.ents[name]
	if !ok {
		return syscall.ENOENT
	}
	n := m.nodes[ino]
	isDir := n.mode&format.SIFMT == format.SIFDIR
	if wantDir {
		if !isDir {
			return syscall.ENOTDIR
		}
		if len(n.ents) > 2 {
			return syscall.ENOTEMPTY
		}
	} else if isDir {
		return syscall.EISDIR
	}
	now := m.Now()
	delete(p.ents, name)
	if wantDir {
		n.nlink = 0
		p.nlink--
	} else {
		n.nlink--
	}
	n.ctime = now
	p.mtime, p.ctime = now, now
	return nil
}

func (m *Model) isAncestor(anc, dir uint64) bool {
	cur := dir
	for steps := 0; steps < 1<<20; steps++ {
		if cur == anc {
			return true
		}
		if cur == 1 {
			return false
		}
		n := m.nodes[cur]
		next, ok := n.ents[".."]
		if !ok || next == cur {
			return false
		}
		cur = next
	}
	return false
}

func (m *Model) Rename(oldparent uint64, oldname string, newparent uint64, newname string, flags uint32) error {
	if err := checkName(oldname); err != nil {
		return err
	}
	if err := checkName(newname); err != nil {
		return err
	}
	if flags&^(engine.RenameNoReplace|engine.RenameExchange) != 0 || flags == engine.RenameNoReplace|engine.RenameExchange {
		return syscall.EINVAL
	}
	if oldname == "." || oldname == ".." || newname == "." || newname == ".." {
		return syscall.EBUSY
	}
	op, err := m.dir(oldparent)
	if err != nil {
		return err
	}
	np, err := m.dir(newparent)
	if err != nil {
		return err
	}
	if np.nlink == 0 {
		return syscall.ENOENT
	}
	sino, ok := op.ents[oldname]
	if !ok {
		return syscall.ENOENT
	}
	dino, dstExists := np.ents[newname]
	exchange := flags&engine.RenameExchange != 0
	if exchange && !dstExists {
		return syscall.ENOENT
	}
	if dstExists && flags&engine.RenameNoReplace != 0 {
		return syscall.EEXIST
	}
	if dstExists && dino == sino && !exchange {
		return nil
	}
	src := m.nodes[sino]
	srcDir := src.mode&format.SIFMT == format.SIFDIR
	if srcDir && m.isAncestor(sino, newparent) {
		return syscall.EINVAL
	}
	var dst *node
	dstDir := false
	if dstExists {
		dst = m.nodes[dino]
		dstDir = dst.mode&format.SIFMT == format.SIFDIR
		if exchange {
			if dstDir && m.isAncestor(dino, oldparent) {
				return syscall.EINVAL
			}
		} else {
			if srcDir && !dstDir {
				return syscall.ENOTDIR
			}
			if !srcDir && dstDir {
				return syscall.EISDIR
			}
			if dstDir && len(dst.ents) > 2 {
				return syscall.ENOTEMPTY
			}
		}
	}
	cross := oldparent != newparent
	if srcDir && cross && !exchange && !(dstExists && dstDir) && np.nlink >= format.MaxLinks {
		return syscall.EMLINK
	}
	now := m.Now()
	if exchange {
		op.ents[oldname] = dino
		np.ents[newname] = sino
		if cross {
			if srcDir {
				src.ents[".."] = newparent
			}
			if dstDir {
				dst.ents[".."] = oldparent
			}
			switch {
			case srcDir && !dstDir:
				op.nlink--
				np.nlink++
			case dstDir && !srcDir:
				np.nlink--
				op.nlink++
			}
		}
		dst.ctime = now
	} else {
		np.ents[newname] = sino
		delete(op.ents, oldname)
		if srcDir && cross {
			src.ents[".."] = newparent
			op.nlink--
			np.nlink++
		}
		if dstExists {
			if dstDir {
				dst.nlink = 0
				np.nlink--
			} else {
				dst.nlink--
			}
			dst.ctime = now
		}
	}
	src.ctime = now
	op.mtime, op.ctime = now, now
	if cross {
		np.mtime, np.ctime = now, now
	}
	return nil
}

func (m *Model) Read(ino uint64, off uint64, p []byte) (int, error) {
	n, err := m.get(ino)
	if err != nil {
		return 0, err
	}
	switch n.mode & format.SIFMT {
	case format.SIFDIR:
		return 0, syscall.EISDIR
	case format.SIFREG:
	default:
		return 0, syscall.EINVAL
	}
	if off >= uint64(len(n.data)) || len(p) == 0 {
		return 0, nil
	}
	return copy(p, n.data[off:]), nil
}

func (m *Model) Write(ctx engine.Ctx, ino uint64, off uint64, data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	n, err := m.get(ino)
	if err != nil {
		return 0, err
	}
	switch n.mode & format.SIFMT {
	case format.SIFDIR:
		return 0, syscall.EISDIR
	case format.SIFREG:
	default:
		return 0, syscall.EINVAL
	}
	if off+uint64(len(data)) > engine.MaxFileSize {
		return 0, syscall.EFBIG
	}
	if end := off + uint64(len(data)); end > uint64(len(n.data)) {
		n.data = append(n.data, make([]byte, end-uint64(len(n.data)))...)
	}
	copy(n.data[off:], data)
	now := m.Now()
	n.mtime, n.ctime = now, now
	if ctx.UID != 0 {
		n.mode &^= sISUID
		if n.mode&sIXGRP != 0 {
			n.mode &^= sISGID
		}
	}
	return len(data), nil
}

func (m *Model) Setattr(ctx engine.Ctx, ino uint64, a engine.SetAttrIn) (engine.Attr, error) {
	n, err := m.get(ino)
	if err != nil {
		return engine.Attr{}, err
	}
	now := m.Now()
	if a.Valid&engine.SetSize != 0 {
		switch n.mode & format.SIFMT {
		case format.SIFDIR:
			return engine.Attr{}, syscall.EISDIR
		case format.SIFREG:
		default:
			return engine.Attr{}, syscall.EINVAL
		}
		if a.Size > engine.MaxFileSize {
			return engine.Attr{}, syscall.EFBIG
		}
		if a.Size >= uint64(len(n.data)) {
			n.data = append(n.data, make([]byte, a.Size-uint64(len(n.data)))...)
		} else {
			n.data = n.data[:a.Size]
		}
		n.mtime, n.ctime = now, now
	}
	if a.Valid&engine.SetMode != 0 {
		n.mode = n.mode&format.SIFMT | a.Mode&0o7777
		n.ctime = now
	}
	if a.Valid&(engine.SetUID|engine.SetGID) != 0 {
		if a.Valid&engine.SetUID != 0 {
			n.uid = a.UID
		}
		if a.Valid&engine.SetGID != 0 {
			n.gid = a.GID
		}
		if n.mode&format.SIFMT == format.SIFREG && ctx.UID != 0 {
			n.mode &^= sISUID
			if n.mode&sIXGRP != 0 {
				n.mode &^= sISGID
			}
		}
		n.ctime = now
	}
	if a.Valid&engine.SetAtime != 0 {
		n.atime = a.Atime
		if a.AtimeNow {
			n.atime = now
		}
		n.ctime = now
	}
	if a.Valid&engine.SetMtime != 0 {
		n.mtime = a.Mtime
		if a.MtimeNow {
			n.mtime = now
		}
		n.ctime = now
	}
	return attr(ino, n), nil
}

func (m *Model) Fallocate(ctx engine.Ctx, ino uint64, mode uint32, off, length uint64) error {
	n, err := m.get(ino)
	if err != nil {
		return err
	}
	switch n.mode & format.SIFMT {
	case format.SIFDIR:
		return syscall.EISDIR
	case format.SIFREG:
	default:
		return syscall.ENODEV
	}
	if length == 0 || off+length < off || off+length > engine.MaxFileSize {
		return syscall.EINVAL
	}
	end := off + length
	keep := mode&engine.FallocKeepSize != 0
	switch mode &^ engine.FallocKeepSize {
	case 0:
		if !keep && end > uint64(len(n.data)) {
			n.data = append(n.data, make([]byte, end-uint64(len(n.data)))...)
		}
		n.ctime = m.Now()
		return nil
	case engine.FallocPunchHole:
		if !keep {
			return syscall.EOPNOTSUPP
		}
		if off >= uint64(len(n.data)) {
			return nil
		}
		end = min(end, uint64(len(n.data)))
		clear(n.data[off:end])
		now := m.Now()
		n.mtime, n.ctime = now, now
		return nil
	}
	return syscall.EOPNOTSUPP
}

// Equal reports whether two files hold identical bytes (test helper).
func Equal(a, b []byte) bool { return bytes.Equal(a, b) }

// Item describes one live path in the model (used by workload generators).
type Item struct {
	Path  string
	Mode  uint32
	Size  uint64
	Nlink uint32
}

// List returns every path reachable from the root, plus the total number of
// regular-file bytes.
func (m *Model) List() (items []Item, totalData uint64) {
	var walk func(ino uint64, path string)
	walk = func(ino uint64, path string) {
		n := m.nodes[ino]
		size := uint64(len(n.data))
		totalData += size
		items = append(items, Item{Path: path, Mode: n.mode, Size: size, Nlink: n.nlink})
		if n.mode&format.SIFMT == format.SIFDIR {
			names := make([]string, 0, len(n.ents))
			for name := range n.ents {
				if name != "." && name != ".." {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			for _, name := range names {
				walk(n.ents[name], path+"/"+name)
			}
		}
	}
	walk(1, "")
	return
}
