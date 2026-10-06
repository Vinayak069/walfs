package crashtest

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"syscall"

	"github.com/vinayak-singh/walfs/internal/engine"
	"github.com/vinayak-singh/walfs/internal/format"
	"github.com/vinayak-singh/walfs/internal/model"
)

// Op is one abstract, path-based filesystem operation.
type Op struct {
	Kind     string
	A, B     string
	Off, Len uint64
	Seed     int64
	Mode     uint32
	Flags    uint32
	Slot     int
	UID, GID uint32
	CtxUID   uint32
	Page     int
	Time     int64
}

func (o Op) String() string {
	return fmt.Sprintf("%s A=%q B=%q off=%d len=%d mode=%o flags=%d slot=%d", o.Kind, o.A, o.B, o.Off, o.Len, o.Mode, o.Flags, o.Slot)
}

// GenConfig shapes the random workload.
type GenConfig struct {
	Seed int64
	// NoOverwrite restricts the workload to operations that never modify
	// bytes of already-allocated blocks in place (appends, sparse extends,
	// block-aligned truncates). Under that restriction ordered mode must
	// still recover to an exact operation-boundary state, data included.
	NoOverwrite bool
	MaxWrite    int    // largest single write, bytes
	MaxFile     uint64 // largest file offset+length
	MaxData     uint64 // soft cap on total regular-file bytes
	NoHandles   bool   // never keep files open
	Names       int    // size of the name pool (default 8)
	SyncPercent int    // chance (per mille) of a sync op
	Sparse      bool   // allow rare writes far past EOF (double-indirect blocks)
	NoSnapshots bool   // never create, delete or read snapshots
	// NoCompound forbids operations that the runner executes as several engine
	// calls (writing to a missing path = create + write). Crash tests need it:
	// a commit may legitimately land between the two calls, which is a valid
	// on-disk state but not an operation boundary of the model.
	NoCompound bool
}

// Gen produces operations.
type Gen struct {
	rng *rand.Rand
	cfg GenConfig
}

func NewGen(cfg GenConfig) *Gen {
	if cfg.MaxWrite == 0 {
		cfg.MaxWrite = 24 << 10
	}
	if cfg.MaxFile == 0 {
		cfg.MaxFile = 1 << 20
	}
	if cfg.MaxData == 0 {
		cfg.MaxData = 2 << 20
	}
	if cfg.Names == 0 {
		cfg.Names = 8
	}
	if cfg.SyncPercent == 0 {
		cfg.SyncPercent = 60
	}
	return &Gen{rng: rand.New(rand.NewSource(cfg.Seed)), cfg: cfg}
}

func (g *Gen) name() string {
	n := g.rng.Intn(g.cfg.Names)
	if g.cfg.Names <= 26 {
		return string(rune('a' + n))
	}
	// Large name pools grow directories past one block; some names are long
	// so that directory entries have very different record lengths.
	if n%7 == 0 {
		return fmt.Sprintf("long-%0*d", 60+n%190, n)
	}
	return fmt.Sprintf("n%d", n)
}

func pick[T any](g *Gen, xs []T) T { return xs[g.rng.Intn(len(xs))] }

func (g *Gen) newPath(dirs []string) string {
	return pick(g, dirs) + "/" + g.name()
}

// Next generates the next operation given the model's current contents.
func (g *Gen) Next(m *model.Model) Op {
	items, total := m.List()
	var dirs, files, all, links, specials []string
	sizes := map[string]uint64{}
	for _, it := range items {
		sizes[it.Path] = it.Size
		switch it.Mode & format.SIFMT {
		case format.SIFDIR:
			dirs = append(dirs, it.Path)
		case format.SIFREG:
			files = append(files, it.Path)
		case format.SIFLNK:
			links = append(links, it.Path)
		default:
			specials = append(specials, it.Path)
		}
		if it.Path != "" {
			all = append(all, it.Path)
		}
	}
	anyPath := func() string {
		if len(all) == 0 || g.rng.Intn(8) == 0 {
			return g.newPath(dirs)
		}
		return pick(g, all)
	}
	filePath := func() string {
		if len(files) == 0 || g.rng.Intn(10) == 0 {
			return anyPath()
		}
		return pick(g, files)
	}
	dstPath := func() string {
		if len(all) > 0 && g.rng.Intn(3) == 0 {
			return pick(g, all)
		}
		return g.newPath(dirs)
	}
	op := Op{CtxUID: uint32(g.rng.Intn(2)) * 1000, Seed: g.rng.Int63(), Page: g.rng.Intn(4)}

	// Keep the working set bounded: reclaim space when over budget.
	if total > g.cfg.MaxData && len(files) > 0 {
		op.A = pick(g, files)
		if g.rng.Intn(2) == 0 {
			op.Kind = "unlink"
		} else {
			op.Kind, op.Off = "truncate", 0
		}
		return op
	}
	if g.rng.Intn(1000) < g.cfg.SyncPercent {
		op.Kind = "sync"
		return op
	}

	type w struct {
		kind string
		wt   int
	}
	table := []w{
		{"create", 12}, {"mkdir", 8}, {"write", 24}, {"truncate", 5}, {"unlink", 7},
		{"rmdir", 4}, {"rename", 11}, {"link", 5}, {"symlink", 4}, {"mknod", 2},
		{"chmod", 2}, {"chown", 2}, {"utimens", 2}, {"fallocate", 3}, {"read", 5},
		{"readdir", 2}, {"stat", 3},
	}
	if !g.cfg.NoSnapshots {
		table = append(table, w{"snap", 3}, w{"snapdel", 2}, w{"snapread", 4})
	}
	if !g.cfg.NoHandles {
		table = append(table, w{"open", 3}, w{"hread", 1}, w{"hclose", 3}, w{"hstat", 1})
		if !g.cfg.NoOverwrite {
			// hwrite writes through a handle whose file differs from the
			// path used to pick the offset, so it cannot honour the
			// append-only guarantee of NoOverwrite workloads.
			table = append(table, w{"hwrite", 4})
		}
	}
	sum := 0
	for _, e := range table {
		sum += e.wt
	}
	r := g.rng.Intn(sum)
	for _, e := range table {
		if r < e.wt {
			op.Kind = e.kind
			break
		}
		r -= e.wt
	}

	switch op.Kind {
	case "create", "mkdir":
		op.A = g.newPath(dirs)
		op.Mode = pick(g, []uint32{0o644, 0o755, 0o600, 0o4755, 0o2755, 0o777})
	case "mknod":
		op.A = g.newPath(dirs)
		op.Mode = pick(g, []uint32{format.SIFIFO, format.SIFSOCK, format.SIFCHR, format.SIFBLK}) | 0o644
		op.Off = uint64(g.rng.Intn(1000))
	case "symlink":
		op.A = g.newPath(dirs)
		if g.rng.Intn(4) == 0 {
			op.B = strings.Repeat("long/target/", 6+g.rng.Intn(40)) // > 60 bytes: block-backed
		} else {
			op.B = pick(g, append([]string{"x", "../y", "/abs"}, all...))
		}
	case "link":
		op.A = filePath()
		op.B = dstPath()
	case "unlink", "rmdir":
		op.A = anyPath()
		if op.Kind == "rmdir" && len(dirs) > 1 && g.rng.Intn(3) != 0 {
			op.A = pick(g, dirs[1:])
		}
	case "rename":
		op.A, op.B = anyPath(), dstPath()
		op.Flags = pick(g, []uint32{0, 0, 0, 0, 0, 0, 0, 0, engine.RenameNoReplace, engine.RenameExchange})
	case "write", "hwrite":
		g.fillWrite(&op, files, sizes)
		if op.Kind == "hwrite" {
			op.Slot = g.rng.Intn(4)
		} else if g.cfg.NoCompound {
			if _, exists := sizes[op.A]; !exists {
				// Writing to a missing path would be create + write: create it.
				op.Kind, op.Mode = "create", 0o644
			}
		}
	case "truncate":
		op.A = filePath()
		sz := sizes[op.A]
		switch g.rng.Intn(3) {
		case 0:
			op.Off = 0
		case 1:
			op.Off = sz + uint64(g.rng.Intn(30000)) // extend
		default:
			op.Off = uint64(g.rng.Int63n(int64(sz) + 1))
		}
		if g.cfg.NoOverwrite && op.Off < sz {
			op.Off = op.Off &^ (format.BlockSize - 1) // block-aligned shrink only
		}
		op.Off = min(op.Off, g.cfg.MaxFile)
	case "chmod":
		op.A = anyPath()
		op.Mode = uint32(g.rng.Intn(0o10000))
	case "chown":
		op.A = anyPath()
		op.UID, op.GID = uint32(g.rng.Intn(3)*500), uint32(g.rng.Intn(3)*500)
		op.Flags = uint32(1 + g.rng.Intn(3)) // bit0 uid, bit1 gid
	case "utimens":
		op.A = anyPath()
		op.Flags = uint32(g.rng.Intn(16)) // bit0 atime, bit1 atime=now, bit2 mtime, bit3 mtime=now
		op.Time = g.rng.Int63n(1 << 40)
		op.Off = uint64(g.rng.Int63n(1 << 40))
	case "fallocate":
		op.A = filePath()
		sz := sizes[op.A]
		op.Off = uint64(g.rng.Int63n(int64(sz) + 16384))
		op.Len = uint64(1 + g.rng.Intn(65536))
		op.Mode = pick(g, []uint32{0, 0, 1, 1, 3})
		if g.cfg.NoOverwrite && op.Mode&2 != 0 {
			op.Mode = 1
		}
		if op.Off+op.Len > g.cfg.MaxFile {
			op.Len = g.cfg.MaxFile - min(op.Off, g.cfg.MaxFile-1)
		}
	case "read":
		op.A = filePath()
		op.Off = uint64(g.rng.Int63n(int64(sizes[op.A]) + 1))
		op.Len = uint64(1 + g.rng.Intn(70000))
	case "snap", "snapdel":
		op.B = fmt.Sprintf("s%d", g.rng.Intn(4))
	case "snapread":
		op.A = filePath()
		op.B = fmt.Sprintf("s%d", g.rng.Intn(4))
		op.Off = uint64(g.rng.Int63n(int64(sizes[op.A]) + 1))
		op.Len = uint64(1 + g.rng.Intn(70000))
	case "readdir":
		op.A = pick(g, dirs)
	case "stat":
		op.A = anyPath()
	case "open":
		op.A = anyPath()
		if len(files) > 0 && g.rng.Intn(10) < 7 {
			op.A = pick(g, files)
		}
		op.Slot = g.rng.Intn(4)
	case "hread":
		op.Slot = g.rng.Intn(4)
		op.Off = uint64(g.rng.Intn(100000))
		op.Len = uint64(1 + g.rng.Intn(20000))
	case "hclose", "hstat":
		op.Slot = g.rng.Intn(4)
	}
	return op
}

func (g *Gen) fillWrite(op *Op, files []string, sizes map[string]uint64) {
	if len(files) == 0 || g.rng.Intn(15) == 0 {
		op.A = "/" + g.name()
	} else {
		op.A = pick(g, files)
	}
	sz := sizes[op.A]
	// Log-uniform length.
	op.Len = uint64(1 + int(float64(g.cfg.MaxWrite)*g.rng.Float64()*g.rng.Float64()))
	switch {
	case g.cfg.NoOverwrite:
		op.Off = sz
		if g.rng.Intn(4) == 0 {
			op.Off += uint64(g.rng.Intn(20000))
		}
		if g.cfg.Sparse && g.rng.Intn(60) == 0 {
			op.Off = max(op.Off, 4096*1100+uint64(g.rng.Intn(5000)))
		}
	default:
		switch r := g.rng.Intn(100); {
		case r < 25:
			op.Off = 0
		case r < 55:
			op.Off = sz
		case r < 97:
			op.Off = uint64(g.rng.Int63n(int64(sz) + 8192))
		default:
			op.Off = uint64(g.rng.Int63n(int64(g.cfg.MaxFile)))
		}
		if g.cfg.Sparse && g.rng.Intn(60) == 0 {
			op.Off = 4096*1100 + uint64(g.rng.Intn(5000))
		}
	}
	if op.Off+op.Len > g.cfg.MaxFile && !(g.cfg.Sparse && op.Off > 4096*1000) {
		if op.Off >= g.cfg.MaxFile {
			op.Off = sz
		}
		op.Len = min(op.Len, g.cfg.MaxFile-min(op.Off, g.cfg.MaxFile-1))
	}
	op.Len = max(op.Len, 1)
}

// ---------------------------------------------------------------------------
// Applying operations

// Result is what an operation produced; two implementations must agree on it.
type Result struct {
	Err  error
	N    int
	Data []byte
	Attr *engine.Attr
	Dir  []string
}

func (r Result) String() string {
	return fmt.Sprintf("err=%v n=%d datalen=%d attr=%v dir=%v", r.Err, r.N, len(r.Data), r.Attr, r.Dir)
}

// Runner applies ops to one VFS and tracks open "file handles".
type Runner struct {
	FS      VFS
	Handles [4]uint64
	Clock   *int64 // current-operation timestamp, shared with the FS under test
}

func genData(seed int64, n uint64) []byte {
	b := make([]byte, n)
	x := uint64(seed)*0x9E3779B97F4A7C15 + 1
	for i := range b {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = byte(x >> 24)
	}
	return b
}

func split(path string) []string {
	var out []string
	for _, c := range strings.Split(path, "/") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// resolve walks path (no symlink following) and returns the inode. All kernel
// references taken on the way are dropped before returning.
func (r *Runner) resolve(path string) (uint64, error) {
	cur := uint64(format.RootIno)
	var held []uint64
	defer func() {
		for _, h := range held {
			r.FS.Forget(h, 1)
		}
	}()
	for _, c := range split(path) {
		a, err := r.FS.Lookup(cur, c)
		if err != nil {
			return 0, err
		}
		held = append(held, a.Ino)
		cur = a.Ino
	}
	return cur, nil
}

func (r *Runner) resolveParent(path string) (parent uint64, base string, err error) {
	comps := split(path)
	if len(comps) == 0 {
		return 0, "", syscall.EINVAL
	}
	parent, err = r.resolve(strings.Join(comps[:len(comps)-1], "/"))
	return parent, comps[len(comps)-1], err
}

func (r *Runner) ctx(op Op) engine.Ctx { return engine.Ctx{UID: op.CtxUID, GID: op.CtxUID} }

// Apply executes op and reports its observable result.
func (r *Runner) Apply(op Op) (res Result) {
	fs := r.FS
	fin := func(a engine.Attr, err error) Result {
		if err == nil {
			fs.Forget(a.Ino, 1)
		}
		return Result{Err: err}
	}
	switch op.Kind {
	case "sync":
		res.Err = fs.Sync()
	case "snap":
		res.Err = fs.CreateSnapshot(op.B)
	case "snapdel":
		res.Err = fs.DeleteSnapshot(op.B)
	case "snapread":
		v, err := fs.SnapshotView(op.B)
		if err != nil {
			return Result{Err: err}
		}
		return (&Runner{FS: v}).Apply(Op{Kind: "read", A: op.A, Off: op.Off, Len: op.Len})
	case "create", "mkdir", "mknod", "symlink":
		p, base, err := r.resolveParent(op.A)
		if err != nil {
			return Result{Err: err}
		}
		switch op.Kind {
		case "create":
			return fin(fs.Mknod(r.ctx(op), p, base, format.SIFREG|op.Mode, 0))
		case "mknod":
			return fin(fs.Mknod(r.ctx(op), p, base, op.Mode, op.Off))
		case "mkdir":
			return fin(fs.Mkdir(r.ctx(op), p, base, op.Mode))
		default:
			return fin(fs.Symlink(r.ctx(op), p, base, []byte(op.B)))
		}
	case "link":
		ino, err := r.resolve(op.A)
		if err != nil {
			return Result{Err: err}
		}
		p, base, err := r.resolveParent(op.B)
		if err != nil {
			return Result{Err: err}
		}
		return fin(fs.Link(ino, p, base))
	case "unlink", "rmdir":
		p, base, err := r.resolveParent(op.A)
		if err != nil {
			return Result{Err: err}
		}
		if op.Kind == "unlink" {
			res.Err = fs.Unlink(p, base)
		} else {
			res.Err = fs.Rmdir(p, base)
		}
	case "rename":
		op1, n1, err := r.resolveParent(op.A)
		if err != nil {
			return Result{Err: err}
		}
		np, n2, err := r.resolveParent(op.B)
		if err != nil {
			return Result{Err: err}
		}
		res.Err = fs.Rename(op1, n1, np, n2, op.Flags)
	case "write", "hwrite":
		var ino uint64
		var err error
		if op.Kind == "hwrite" {
			if ino = r.Handles[op.Slot]; ino == 0 {
				return Result{Err: syscall.EBADF}
			}
		} else {
			if ino, err = r.resolve(op.A); err == syscall.ENOENT && !strings.Contains(op.A[1:], "/") {
				// Writing to a missing file: create it first (open O_CREAT).
				p, base, perr := r.resolveParent(op.A)
				if perr != nil {
					return Result{Err: perr}
				}
				a, cerr := fs.Mknod(r.ctx(op), p, base, format.SIFREG|0o644, 0)
				if cerr != nil {
					return Result{Err: cerr}
				}
				fs.Forget(a.Ino, 1)
				ino, err = a.Ino, nil
			}
			if err != nil {
				return Result{Err: err}
			}
		}
		res.N, res.Err = fs.Write(r.ctx(op), ino, op.Off, genData(op.Seed, op.Len))
	case "truncate":
		ino, err := r.resolve(op.A)
		if err != nil {
			return Result{Err: err}
		}
		_, res.Err = fs.Setattr(r.ctx(op), ino, engine.SetAttrIn{Valid: engine.SetSize, Size: op.Off})
	case "chmod":
		ino, err := r.resolve(op.A)
		if err != nil {
			return Result{Err: err}
		}
		_, res.Err = fs.Setattr(r.ctx(op), ino, engine.SetAttrIn{Valid: engine.SetMode, Mode: op.Mode})
	case "chown":
		ino, err := r.resolve(op.A)
		if err != nil {
			return Result{Err: err}
		}
		a := engine.SetAttrIn{UID: op.UID, GID: op.GID}
		if op.Flags&1 != 0 {
			a.Valid |= engine.SetUID
		}
		if op.Flags&2 != 0 {
			a.Valid |= engine.SetGID
		}
		_, res.Err = fs.Setattr(r.ctx(op), ino, a)
	case "utimens":
		ino, err := r.resolve(op.A)
		if err != nil {
			return Result{Err: err}
		}
		a := engine.SetAttrIn{}
		if op.Flags&1 != 0 {
			a.Valid |= engine.SetAtime
			a.Atime = op.Time
			a.AtimeNow = op.Flags&2 != 0
		}
		if op.Flags&4 != 0 {
			a.Valid |= engine.SetMtime
			a.Mtime = int64(op.Off)
			a.MtimeNow = op.Flags&8 != 0
		}
		_, res.Err = fs.Setattr(r.ctx(op), ino, a)
	case "fallocate":
		ino, err := r.resolve(op.A)
		if err != nil {
			return Result{Err: err}
		}
		res.Err = fs.Fallocate(r.ctx(op), ino, op.Mode, op.Off, op.Len)
	case "read", "hread":
		var ino uint64
		var err error
		if op.Kind == "hread" {
			if ino = r.Handles[op.Slot]; ino == 0 {
				return Result{Err: syscall.EBADF}
			}
		} else if ino, err = r.resolve(op.A); err != nil {
			return Result{Err: err}
		}
		buf := make([]byte, op.Len)
		res.N, res.Err = fs.Read(ino, op.Off, buf)
		res.Data = buf[:max(res.N, 0)]
	case "readdir":
		ino, err := r.resolve(op.A)
		if err != nil {
			return Result{Err: err}
		}
		ents, err := fs.ReaddirAll(ino, op.Page)
		res.Err = err
		for _, e := range ents {
			res.Dir = append(res.Dir, fmt.Sprintf("%s:%d", e.Name, e.Type))
		}
		sortStrings(res.Dir)
	case "stat", "hstat":
		var ino uint64
		var err error
		if op.Kind == "hstat" {
			if ino = r.Handles[op.Slot]; ino == 0 {
				return Result{Err: syscall.EBADF}
			}
		} else if ino, err = r.resolve(op.A); err != nil {
			return Result{Err: err}
		}
		a, err := fs.Getattr(ino)
		a.Ino, a.Blocks, a.Gen = 0, 0, 0
		if a.Mode&format.SIFMT == format.SIFDIR {
			a.Size = 0
		}
		res.Err, res.Attr = err, &a
	case "open":
		ino, err := r.resolve(op.A)
		if err != nil {
			return Result{Err: err}
		}
		// Take a kernel reference that outlives path resolution.
		if err := r.hold(ino, op.Slot); err != nil {
			return Result{Err: err}
		}
	case "hclose":
		if ino := r.Handles[op.Slot]; ino != 0 {
			fs.Forget(ino, 1)
			r.Handles[op.Slot] = 0
		} else {
			res.Err = syscall.EBADF
		}
	default:
		panic("unknown op " + op.Kind)
	}
	return res
}

// hold stores ino in a handle slot, taking one extra kernel reference by
// looking the inode up by number through its own (re-resolved) path is not
// possible, so the engine provides Ref and the model ignores references.
func (r *Runner) hold(ino uint64, slot int) error {
	if old := r.Handles[slot]; old != 0 {
		r.FS.Forget(old, 1)
	}
	if e, ok := r.FS.(interface{ Ref(uint64) }); ok {
		e.Ref(ino)
	}
	r.Handles[slot] = ino
	return nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ReleaseAll drops every held handle (what the kernel does at unmount).
func (r *Runner) ReleaseAll() {
	for i, h := range r.Handles {
		if h != 0 {
			r.FS.Forget(h, 1)
			r.Handles[i] = 0
		}
	}
}

// SameResult reports whether two results are observably identical.
func SameResult(a, b Result) bool {
	if !errors.Is(a.Err, b.Err) && !(a.Err == nil && b.Err == nil) {
		var ea, eb syscall.Errno
		if !(errors.As(a.Err, &ea) && errors.As(b.Err, &eb) && ea == eb) {
			return false
		}
	}
	if a.N != b.N || string(a.Data) != string(b.Data) || len(a.Dir) != len(b.Dir) {
		return false
	}
	for i := range a.Dir {
		if a.Dir[i] != b.Dir[i] {
			return false
		}
	}
	if (a.Attr == nil) != (b.Attr == nil) {
		return false
	}
	return a.Attr == nil || *a.Attr == *b.Attr
}
