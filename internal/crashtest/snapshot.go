package crashtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"

	"github.com/vinayak-singh/walfs/internal/engine"
	"github.com/vinayak-singh/walfs/internal/format"
)

// Node is one entry of a snapshot.
type Node struct {
	Path                string
	Mode                uint32
	UID, GID, Nlink     uint32
	Size                uint64
	Atime, Mtime, Ctime int64
	Rdev                uint64
	Link                string // symlink target
	Data                []byte // regular-file contents (nil after Compact)
	DataSum             [32]byte
	Group               string // first path (sorted) sharing this inode: hard-link identity
}

// Snapshot is a canonical, implementation-independent view of a whole
// filesystem tree.
type Snapshot struct {
	Nodes  map[string]*Node
	Paths  []string // sorted
	digest [32]byte
	hashed bool
}

// TakeSnapshot walks the tree from the root of fs.
func TakeSnapshot(fs VFS, withTimes bool) (*Snapshot, error) {
	s := &Snapshot{Nodes: map[string]*Node{}}
	byIno := map[uint64][]string{}
	var walk func(ino uint64, path string) error
	walk = func(ino uint64, path string) error {
		a, err := fs.Getattr(ino)
		if err != nil {
			return fmt.Errorf("getattr %q: %w", path, err)
		}
		n := &Node{
			Path: path, Mode: a.Mode, UID: a.UID, GID: a.GID, Nlink: a.Nlink,
			Size: a.Size, Mtime: a.Mtime, Ctime: a.Ctime, Atime: a.Atime, Rdev: a.Rdev,
		}
		if !withTimes {
			n.Atime, n.Mtime, n.Ctime = 0, 0, 0
		}
		isDir := a.Mode&format.SIFMT == format.SIFDIR
		if isDir {
			n.Size = 0
		}
		s.Nodes[path] = n
		byIno[ino] = append(byIno[ino], path)
		switch a.Mode & format.SIFMT {
		case format.SIFREG:
			n.Data = make([]byte, a.Size)
			for off := uint64(0); off < a.Size; {
				k, err := fs.Read(ino, off, n.Data[off:min(off+1<<16, a.Size)])
				if err != nil {
					return fmt.Errorf("read %q: %w", path, err)
				}
				if k == 0 {
					return fmt.Errorf("read %q: short read at %d of %d", path, off, a.Size)
				}
				off += uint64(k)
			}
			n.DataSum = sha256.Sum256(n.Data)
		case format.SIFLNK:
			t, err := fs.Readlink(ino)
			if err != nil {
				return fmt.Errorf("readlink %q: %w", path, err)
			}
			n.Link = string(t)
		case format.SIFDIR:
			ents, err := fs.ReaddirAll(ino, 0)
			if err != nil {
				return fmt.Errorf("readdir %q: %w", path, err)
			}
			seen := map[string]bool{}
			for _, e := range ents {
				if seen[e.Name] {
					return fmt.Errorf("readdir %q: duplicate entry %q", path, e.Name)
				}
				seen[e.Name] = true
			}
			sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
			for _, e := range ents {
				if e.Name == "." || e.Name == ".." {
					continue
				}
				if err := walk(e.Ino, path+"/"+e.Name); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(format.RootIno, ""); err != nil {
		return nil, err
	}
	for _, paths := range byIno {
		sort.Strings(paths)
		for _, p := range paths {
			s.Nodes[p].Group = paths[0]
		}
	}
	for p := range s.Nodes {
		s.Paths = append(s.Paths, p)
	}
	sort.Strings(s.Paths)
	return s, nil
}

// Digest returns a hash of the canonical form, including file contents.
func (s *Snapshot) Digest() [32]byte { return s.digestOpt(true) }

// DigestNoData hashes everything except regular-file contents (sizes and
// all metadata are still covered).
func (s *Snapshot) DigestNoData() [32]byte { return s.digestOpt(false) }

func (s *Snapshot) digestOpt(withData bool) [32]byte {
	if withData && s.hashed {
		return s.digest
	}
	h := sha256.New()
	var tmp [8]byte
	put := func(v uint64) { binary.LittleEndian.PutUint64(tmp[:], v); h.Write(tmp[:]) }
	for _, p := range s.Paths {
		n := s.Nodes[p]
		h.Write([]byte(p))
		h.Write([]byte{0})
		put(uint64(n.Mode))
		put(uint64(n.UID)<<32 | uint64(n.GID))
		put(uint64(n.Nlink))
		put(n.Size)
		put(uint64(n.Atime))
		put(uint64(n.Mtime))
		put(uint64(n.Ctime))
		put(n.Rdev)
		h.Write([]byte(n.Link))
		h.Write([]byte{0})
		h.Write([]byte(n.Group))
		h.Write([]byte{0})
		if withData {
			h.Write(n.DataSum[:])
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	if withData {
		s.digest, s.hashed = out, true
	}
	return out
}

// Diff describes up to a handful of differences between two snapshots, or ""
// if they are identical. a is "expected", b is "actual".
func Diff(a, b *Snapshot) string {
	var out []string
	add := func(f string, args ...any) {
		if len(out) < 12 {
			out = append(out, fmt.Sprintf(f, args...))
		}
	}
	for _, p := range a.Paths {
		x := a.Nodes[p]
		y := b.Nodes[p]
		if y == nil {
			add("missing %q", p)
			continue
		}
		if x.Mode != y.Mode {
			add("%q: mode %o != %o", p, x.Mode, y.Mode)
		}
		if x.UID != y.UID || x.GID != y.GID {
			add("%q: owner %d:%d != %d:%d", p, x.UID, x.GID, y.UID, y.GID)
		}
		if x.Nlink != y.Nlink {
			add("%q: nlink %d != %d", p, x.Nlink, y.Nlink)
		}
		if x.Size != y.Size {
			add("%q: size %d != %d", p, x.Size, y.Size)
		}
		if x.Atime != y.Atime || x.Mtime != y.Mtime || x.Ctime != y.Ctime {
			add("%q: times a/m/c %d/%d/%d != %d/%d/%d", p, x.Atime, x.Mtime, x.Ctime, y.Atime, y.Mtime, y.Ctime)
		}
		if x.Rdev != y.Rdev {
			add("%q: rdev %d != %d", p, x.Rdev, y.Rdev)
		}
		if x.Link != y.Link {
			add("%q: symlink %q != %q", p, x.Link, y.Link)
		}
		if x.Group != y.Group {
			add("%q: hard-link group %q != %q", p, x.Group, y.Group)
		}
		if x.DataSum != y.DataSum {
			if x.Data != nil && y.Data != nil && !bytes.Equal(x.Data, y.Data) {
				add("%q: contents differ (%s)", p, firstDiff(x.Data, y.Data))
			} else {
				add("%q: contents differ", p)
			}
		}
	}
	for _, p := range b.Paths {
		if a.Nodes[p] == nil {
			add("unexpected %q", p)
		}
	}
	return strings.Join(out, "\n")
}

func firstDiff(a, b []byte) string {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			cnt := 0
			for j := i; j < n; j++ {
				if a[j] != b[j] {
					cnt++
				}
			}
			return fmt.Sprintf("first at byte %d (want %#x got %#x), %d differing bytes, lens %d/%d", i, a[i], b[i], cnt, len(a), len(b))
		}
	}
	return fmt.Sprintf("lengths %d/%d", len(a), len(b))
}

var _ = engine.Ctx{}

// Compact drops file contents (keeping their hashes) to save memory.
func (s *Snapshot) Compact() {
	for _, n := range s.Nodes {
		n.Data = nil
	}
}

// Full is a main-tree snapshot plus the tree of every filesystem snapshot.
type Full struct {
	Main  *Snapshot
	Snaps map[string]*Snapshot
}

// TakeFull walks the live tree and every snapshot view.
func TakeFull(fs VFS, withTimes bool) (*Full, error) {
	f := &Full{Snaps: map[string]*Snapshot{}}
	var err error
	if f.Main, err = TakeSnapshot(fs, withTimes); err != nil {
		return nil, err
	}
	for _, name := range fs.SnapshotNames() {
		v, err := fs.SnapshotView(name)
		if err != nil {
			return nil, fmt.Errorf("snapshot %q: %w", name, err)
		}
		if f.Snaps[name], err = TakeSnapshot(v, withTimes); err != nil {
			return nil, fmt.Errorf("snapshot %q: %w", name, err)
		}
	}
	return f, nil
}

func (f *Full) names() []string {
	var out []string
	for n := range f.Snaps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (f *Full) digestOpt(withData bool) [32]byte {
	h := sha256.New()
	d := f.Main.digestOpt(withData)
	h.Write(d[:])
	for _, n := range f.names() {
		h.Write([]byte("\x00snap:" + n + "\x00"))
		d := f.Snaps[n].digestOpt(withData)
		h.Write(d[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (f *Full) Digest() [32]byte       { return f.digestOpt(true) }
func (f *Full) DigestNoData() [32]byte { return f.digestOpt(false) }

// Compact drops file contents everywhere (keeping hashes).
func (f *Full) Compact() {
	f.Main.Compact()
	for _, s := range f.Snaps {
		s.Compact()
	}
}

// DiffFull describes differences between two full snapshots.
func DiffFull(want, got *Full) string {
	var out []string
	if d := Diff(want.Main, got.Main); d != "" {
		out = append(out, "main tree:\n"+d)
	}
	for _, n := range want.names() {
		g, ok := got.Snaps[n]
		if !ok {
			out = append(out, fmt.Sprintf("snapshot %q missing", n))
			continue
		}
		if d := Diff(want.Snaps[n], g); d != "" {
			out = append(out, fmt.Sprintf("snapshot %q:\n%s", n, d))
		}
	}
	for _, n := range got.names() {
		if _, ok := want.Snaps[n]; !ok {
			out = append(out, fmt.Sprintf("unexpected snapshot %q", n))
		}
	}
	return strings.Join(out, "\n")
}
