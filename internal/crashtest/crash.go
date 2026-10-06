package crashtest

import (
	"fmt"
	"math/rand"
	"runtime"
	"sort"
	"sync"

	"github.com/vinayak-singh/walfs/internal/blockdev"
	"github.com/vinayak-singh/walfs/internal/engine"
	"github.com/vinayak-singh/walfs/internal/format"
	"github.com/vinayak-singh/walfs/internal/fsck"
	"github.com/vinayak-singh/walfs/internal/model"
)

// CrashConfig configures a power-failure simulation.
type CrashConfig struct {
	Gen           GenConfig
	Ops           int
	Mode          engine.JournalMode
	ImageBlocks   int64
	JournalBlocks uint64
	CacheBlocks   int
	DirtyLimit    int

	// ExactData requires recovered file contents to equal a model state byte
	// for byte. Ordered mode only guarantees that for workloads that never
	// overwrite allocated blocks in place (see GenConfig.NoOverwrite);
	// otherwise only metadata and sizes are compared.
	ExactData bool

	PointStride     int   // test every Nth crash point (1 = all)
	SubsetsPerPoint int   // random persisted-subsets tried at each point
	RecoveryEvery   int   // every Nth image also crashes mid-recovery (0 = never)
	DropFlushes     bool  // negative control: the device ignores flushes
	CrashSeed       int64 // randomness of subset selection
	Workers         int
	MaxFailures     int
}

// DefaultCrash returns a configuration tuned for CI speed.
func DefaultCrash(seed int64, mode engine.JournalMode) CrashConfig {
	cfg := CrashConfig{
		Gen:             GenConfig{Seed: seed, MaxWrite: 20 << 10, MaxFile: 256 << 10, MaxData: 512 << 10, Names: 6, Sparse: true, SyncPercent: 150},
		Ops:             250,
		Mode:            mode,
		ImageBlocks:     2048,
		JournalBlocks:   128,
		CacheBlocks:     96,
		DirtyLimit:      24,
		PointStride:     1,
		SubsetsPerPoint: 4,
		RecoveryEvery:   16,
		CrashSeed:       seed,
		MaxFailures:     5,
	}
	switch mode {
	case engine.ModeData:
		cfg.ExactData = true
	case engine.ModeOrdered:
		cfg.Gen.NoOverwrite = true
		cfg.ExactData = true
	}
	return cfg
}

// CrashReport summarises a simulation.
type CrashReport struct {
	Events, Flushes   int
	Points, Images    int
	RecoveryImages    int
	Failures          []string
	TotalFailures     int
	Checkpoints       uint64
	Commits           uint64
	StatesSeen        int // distinct operation-boundary states observed after recovery
	MinK, MaxK        int
	ReplayedTxnImages int // images whose recovery replayed at least one transaction
}

func (r *CrashReport) OK() bool { return r.TotalFailures == 0 }

func (r *CrashReport) String() string {
	return fmt.Sprintf("%d events (%d flushes), %d crash points, %d images (+%d crash-during-recovery), "+
		"%d replayed a journal, %d distinct recovered states, %d commits/%d checkpoints, %d failures",
		r.Events, r.Flushes, r.Points, r.Images, r.RecoveryImages, r.ReplayedTxnImages, r.StatesSeen,
		r.Commits, r.Checkpoints, r.TotalFailures)
}

type session struct {
	cfg     CrashConfig
	rec     *blockdev.RecordingDevice
	digests [][32]byte  // digests[k]: expected state after k completed ops
	snaps   []*Snapshot // compacted snapshots, for failure diagnostics
	opText  []string
	// minK[p]: lowest acceptable k for a crash after p events; computed from
	// operations that completed (including their sync) by then.
	syncs []syncMark
	tags  []int // op index of each event (-1 for mount-time)
}

type syncMark struct{ ev, k int } // after ev events, state k is durable

func (cfg *CrashConfig) opts(clock func() int64) engine.Options {
	return engine.Options{Mode: cfg.Mode, Clock: clock, CacheBlocks: cfg.CacheBlocks, DirtyLimitBlocks: cfg.DirtyLimit}
}

func fixedClock() int64 { return 1 << 50 }

// RunCrash records one workload and then crash-tests it.
func RunCrash(cfg CrashConfig) (*CrashReport, error) {
	if cfg.Workers == 0 {
		cfg.Workers = runtime.NumCPU()
	}
	if cfg.MaxFailures == 0 {
		cfg.MaxFailures = 5
	}
	mem := blockdev.NewMem(cfg.ImageBlocks * format.BlockSize)
	if _, err := format.Mkfs(mem, format.MkfsOptions{AssumeZero: true, JournalBlocks: cfg.JournalBlocks, Inodes: 256, Now: 1000}); err != nil {
		return nil, err
	}
	rec := blockdev.NewRecording(mem)
	rec.DropFlushes = cfg.DropFlushes
	clk := &clockVar{v: 1000}
	rec.SetTag(-1)
	fs, _, err := engine.Open(rec, cfg.opts(clk.now))
	if err != nil {
		return nil, err
	}
	m := model.New(clk.now)
	mr := &Runner{FS: ModelVFS{m}}
	er := &Runner{FS: EngineVFS{fs}}
	g := NewGen(cfg.Gen)
	ses := &session{cfg: cfg, rec: rec}
	snap0, err := TakeSnapshot(ModelVFS{m}, true)
	if err != nil {
		return nil, err
	}
	ses.digests = append(ses.digests, ses.digest(snap0))
	snap0.Compact()
	ses.snaps = append(ses.snaps, snap0)
	for i := 0; i < cfg.Ops; i++ {
		clk.v = 1000 + int64(i)*10
		rec.SetTag(i)
		op := g.Next(m)
		rEng := er.Apply(op)
		rMod := mr.Apply(op)
		if !SameResult(rEng, rMod) {
			return nil, fmt.Errorf("op %d %s: engine/model diverged before any crash:\n  engine %s\n  model  %s", i, op, rEng, rMod)
		}
		if a := fs.Aborted(); a != nil {
			return nil, fmt.Errorf("op %d: engine aborted: %v", i, a)
		}
		s, err := TakeSnapshot(ModelVFS{m}, true)
		if err != nil {
			return nil, err
		}
		ses.digests = append(ses.digests, ses.digest(s))
		s.Compact()
		ses.snaps = append(ses.snaps, s)
		ses.opText = append(ses.opText, op.String())
		if op.Kind == "sync" && rEng.Err == nil {
			ses.syncs = append(ses.syncs, syncMark{ev: len(rec.Log()), k: i + 1})
		}
	}
	st := fs.Stats()
	rep := &CrashReport{Commits: st.Commits, Checkpoints: st.Checkpoints}
	log := rec.Log()
	for _, e := range log {
		ses.tags = append(ses.tags, e.Tag)
		if e.Kind == blockdev.OpFlush {
			rep.Flushes++
		}
	}
	rep.Events = len(log)
	ses.run(rep)
	if len(rep.Failures) > 0 {
		sort.Strings(rep.Failures)
	}
	return rep, nil
}

func (s *session) digest(sn *Snapshot) [32]byte {
	if s.cfg.ExactData {
		return sn.Digest()
	}
	return sn.DigestNoData()
}

// window returns the acceptable range [lo,hi] of recovered operation counts
// for a crash after p events.
func (s *session) window(p int) (lo, hi int) {
	for _, m := range s.syncs {
		if m.ev <= p && m.k > lo {
			lo = m.k
		}
	}
	// The newest operation that could have started: the tag of the last
	// event (or, with no events yet in this op, the previous one) bounds the
	// number of operations whose effects may be on disk. A sync in operation
	// i commits operations 0..i, i.e. state k=i+1.
	hi = len(s.digests) - 1
	if p > 0 {
		if t := s.tags[p-1]; t+1 < hi {
			hi = max(t+1, 0)
		}
	} else {
		hi = 0
	}
	// A completed sync pins state lo as durable even if it logged no events
	// of its own (its effects were already committed by earlier events).
	return lo, max(hi, lo)
}

type job struct {
	img     *blockdev.MemDevice
	point   int
	variant string
	recov   bool
}

func (s *session) run(rep *CrashReport) {
	log := s.rec.Log()
	rng := rand.New(rand.NewSource(s.cfg.CrashSeed))
	durable := blockdev.NewMemFrom(s.rec.Base())
	var pending []blockdev.LogEntry

	jobs := make(chan job, s.cfg.Workers*2)
	var mu sync.Mutex
	seenStates := map[int]bool{}
	var wg sync.WaitGroup
	fail := func(j job, f string, a ...any) {
		mu.Lock()
		rep.TotalFailures++
		if len(rep.Failures) < s.cfg.MaxFailures {
			rep.Failures = append(rep.Failures, fmt.Sprintf("crash after event %d/%d (%s): %s", j.point, len(log), j.variant, fmt.Sprintf(f, a...)))
		}
		mu.Unlock()
	}
	for w := 0; w < s.cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				s.verify(j, rep, &mu, seenStates, fail)
			}
		}()
	}
	imgCount := 0
	for p := 0; p <= len(log); p++ {
		if p%s.cfg.PointStride == 0 {
			mu.Lock()
			rep.Points++
			mu.Unlock()
			variants := []string{"none", "all"}
			for k := 0; k < s.cfg.SubsetsPerPoint; k++ {
				variants = append(variants, "random")
			}
			for _, v := range variants {
				img := blockdev.NewMemFrom(durable.Image())
				desc := applyPending(img, pending, v, rng)
				imgCount++
				j := job{img: img, point: p, variant: desc}
				if s.cfg.RecoveryEvery > 0 && imgCount%s.cfg.RecoveryEvery == 0 {
					j.recov = true
				}
				mu.Lock()
				rep.Images++
				mu.Unlock()
				jobs <- j
				mu.Lock()
				tooMany := rep.TotalFailures >= s.cfg.MaxFailures*4
				mu.Unlock()
				if tooMany {
					break
				}
			}
		}
		if p == len(log) {
			break
		}
		switch e := log[p]; e.Kind {
		case blockdev.OpFlush:
			for _, w := range pending {
				_, _ = durable.WriteAt(w.Data, w.Off)
			}
			pending = pending[:0]
		default:
			pending = append(pending, e)
		}
	}
	close(jobs)
	wg.Wait()
	rep.StatesSeen = len(seenStates)
}

// applyPending persists a chosen subset of the not-yet-flushed writes. Each
// chosen write may additionally be torn at 512-byte sector granularity, which
// is the atomicity unit real disks promise.
func applyPending(img *blockdev.MemDevice, pending []blockdev.LogEntry, variant string, rng *rand.Rand) string {
	switch variant {
	case "none":
		return "no unflushed writes persisted"
	case "all":
		for _, w := range pending {
			_, _ = img.WriteAt(w.Data, w.Off)
		}
		return "all unflushed writes persisted"
	}
	prob := rng.Float64()
	tear := rng.Float64() * 0.5
	kept, torn := 0, 0
	for _, w := range pending {
		if rng.Float64() >= prob {
			continue
		}
		kept++
		if len(w.Data) >= format.SectorSize && rng.Float64() < tear {
			torn++
			for off := 0; off < len(w.Data); off += format.SectorSize {
				if rng.Intn(2) == 0 {
					end := min(off+format.SectorSize, len(w.Data))
					_, _ = img.WriteAt(w.Data[off:end], w.Off+int64(off))
				}
			}
			continue
		}
		_, _ = img.WriteAt(w.Data, w.Off)
	}
	return fmt.Sprintf("random subset: %d/%d writes (p=%.2f), %d torn", kept, len(pending), prob, torn)
}

// verify mounts a crash image, checks it, and optionally crashes the
// recovery itself.
func (s *session) verify(j job, rep *CrashReport, mu *sync.Mutex, seen map[int]bool, fail func(job, string, ...any)) {
	lo, hi := s.window(j.point)
	var before [32]byte
	have := false
	image := j.img.Image()

	clone := blockdev.NewMemFrom(image)
	k, ok := s.mountCheck(clone, j, lo, hi, fail, &before, &have, rep, mu)
	if !ok {
		return
	}
	mu.Lock()
	seen[k] = true
	mu.Unlock()
	if !j.recov {
		return
	}

	// Crash during recovery: record every write the mount performs, then cut
	// power at every point of it.
	base := blockdev.NewMemFrom(image)
	rec := blockdev.NewRecording(base)
	if fs, _, err := engine.Open(rec, s.cfg.opts(fixedClock)); err != nil {
		fail(j, "recording recovery: %v", err)
		return
	} else {
		_ = fs
	}
	rlog := rec.Log()
	durable := blockdev.NewMemFrom(rec.Base())
	var pending []blockdev.LogEntry
	rng := rand.New(rand.NewSource(int64(j.point)*7919 + int64(len(rlog))))
	for p := 0; p <= len(rlog); p++ {
		for _, v := range []string{"none", "all", "random"} {
			img := blockdev.NewMemFrom(durable.Image())
			desc := applyPending(img, pending, v, rng)
			rj := job{img: img, point: j.point, variant: fmt.Sprintf("%s; then crash %d/%d into recovery (%s)", j.variant, p, len(rlog), desc)}
			mu.Lock()
			rep.RecoveryImages++
			mu.Unlock()
			var b2 [32]byte
			have2 := true
			b2 = before
			s.mountCheck(img, rj, lo, hi, fail, &b2, &have2, rep, mu)
		}
		if p == len(rlog) {
			break
		}
		if e := rlog[p]; e.Kind == blockdev.OpFlush {
			for _, w := range pending {
				_, _ = durable.WriteAt(w.Data, w.Off)
			}
			pending = pending[:0]
		} else {
			pending = append(pending, e)
		}
	}
}

// mountCheck mounts img and verifies it. If *have is set, the recovered
// state must equal *want (idempotent recovery); otherwise it must equal one
// of the model states in [lo,hi] and the matching digest is stored.
func (s *session) mountCheck(img *blockdev.MemDevice, j job, lo, hi int, fail func(job, string, ...any),
	want *[32]byte, have *bool, rep *CrashReport, mu *sync.Mutex) (int, bool) {

	fs, info, err := engine.Open(img, s.cfg.opts(fixedClock))
	if err != nil {
		fail(j, "mount failed: %v", err)
		return 0, false
	}
	if info.Journal.Txns > 0 {
		mu.Lock()
		rep.ReplayedTxnImages++
		mu.Unlock()
	}
	snap, err := TakeSnapshot(EngineVFS{fs}, true)
	if err != nil {
		fail(j, "snapshot after recovery: %v", err)
		return 0, false
	}
	d := s.digest(snap)
	k := -1
	if *have {
		if d != *want {
			fail(j, "recovery is not idempotent: second recovery produced a different tree")
			return 0, false
		}
		k = -2
	} else {
		for i := lo; i <= hi && i < len(s.digests); i++ {
			if s.digests[i] == d {
				k = i
				break
			}
		}
		if k < 0 {
			fail(j, "recovered tree matches no operation-boundary state in [%d,%d] (%d files); closest diagnostics: %s",
				lo, hi, len(snap.Paths), s.explain(snap, lo, hi))
			return 0, false
		}
		*want, *have = d, true
	}
	if err := fs.Close(); err != nil {
		fail(j, "unmount after recovery: %v", err)
		return 0, false
	}
	rp, err := fsck.Check(img, fsck.Options{})
	if err != nil {
		fail(j, "fsck error: %v", err)
		return 0, false
	}
	if !rp.OK() {
		fail(j, "fsck after recovery: %s", rp)
		return 0, false
	}
	return k, true
}

// explain diffs a bad snapshot against the nearest acceptable state.
func (s *session) explain(snap *Snapshot, lo, hi int) string {
	best, bestK := "", -1
	for k := lo; k <= hi && k < len(s.snaps); k++ {
		d := Diff(s.snaps[k], snap)
		if bestK < 0 || len(d) < len(best) {
			best, bestK = d, k
		}
	}
	prev, next := "", ""
	if bestK >= 1 {
		prev = s.opText[bestK-1]
	}
	if bestK >= 0 && bestK < len(s.opText) {
		next = s.opText[bestK]
	}
	return fmt.Sprintf("nearest is state k=%d (after op [%s], before op [%s]); expected(first) vs actual(second):\n%s", bestK, prev, next, best)
}
