// Command walfs creates, mounts and checks walfs filesystem images.
//
//	walfs mkfs  [-size 1G] [-journal-blocks N] [-inodes N] IMAGE
//	walfs mount [-journal ordered|data|none] [-commit 5s] MOUNTPOINT IMAGE
//	walfs fsck  [-replay] IMAGE
//	walfs info  IMAGE
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/vinayak-singh/walfs/internal/blockdev"
	"github.com/vinayak-singh/walfs/internal/engine"
	"github.com/vinayak-singh/walfs/internal/format"
	"github.com/vinayak-singh/walfs/internal/fsck"
	"github.com/vinayak-singh/walfs/internal/fusefs"
	"github.com/vinayak-singh/walfs/internal/metrics"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  walfs mkfs  [-size 1G] [-journal-blocks N] [-inodes N] IMAGE
  walfs mount [-journal ordered|data|none] [-commit 5s] [-cache-mb 128] MOUNTPOINT IMAGE
  walfs fsck  [-replay] IMAGE
  walfs info  IMAGE
  walfs snapshot create|delete|list IMAGE [NAME]        (image must not be mounted)

On a mounted filesystem use the hidden directory instead:
  mkdir MOUNT/.snapshots/NAME      create a snapshot
  ls MOUNT/.snapshots/NAME         browse it (read-only)
  rmdir MOUNT/.snapshots/NAME      delete it
`)
	os.Exit(2)
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "walfs: "+f+"\n", a...)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "mkfs":
		cmdMkfs(os.Args[2:])
	case "mount":
		cmdMount(os.Args[2:])
	case "fsck":
		cmdFsck(os.Args[2:])
	case "info":
		cmdInfo(os.Args[2:])
	case "snapshot":
		cmdSnapshot(os.Args[2:])
	default:
		usage()
	}
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for suf, m := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40} {
		if strings.HasSuffix(s, suf) {
			mult, s = m, strings.TrimSuffix(s, suf)
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(n * float64(mult)), nil
}

func cmdMkfs(args []string) {
	fl := flag.NewFlagSet("mkfs", flag.ExitOnError)
	size := fl.String("size", "1G", "image size when IMAGE is a regular file")
	jb := fl.Uint64("journal-blocks", 0, "journal size in 4 KiB blocks (default: 1/32 of the device, 128..32768)")
	inodes := fl.Uint("inodes", 0, "number of inodes (default: one per 16 KiB)")
	ruid := fl.Int("root-uid", os.Getuid(), "owner uid of the root directory (default: you)")
	rgid := fl.Int("root-gid", os.Getgid(), "owner gid of the root directory (default: your group)")
	_ = fl.Parse(args)
	if fl.NArg() != 1 {
		usage()
	}
	path := fl.Arg(0)
	sz, err := parseSize(*size)
	if err != nil {
		die("bad -size: %v", err)
	}
	var dev blockdev.Device
	if st, err := os.Stat(path); err == nil && !st.Mode().IsRegular() {
		d, err := blockdev.OpenFile(path, 0, false)
		if err != nil {
			die("%v", err)
		}
		dev = d
	} else {
		d, err := blockdev.CreateFile(path, sz)
		if err != nil {
			die("%v", err)
		}
		dev = d
	}
	defer dev.Close()
	sb, err := format.Mkfs(dev, format.MkfsOptions{
		Inodes: uint32(*inodes), JournalBlocks: *jb, RootUID: uint32(*ruid), RootGID: uint32(*rgid),
		AssumeZero: func() bool { st, err := os.Stat(path); return err == nil && st.Mode().IsRegular() }(),
	})
	if err != nil {
		die("mkfs: %v", err)
	}
	fmt.Printf("created walfs on %s: %d blocks of %d bytes, %d inodes, %d-block journal\n",
		path, sb.TotalBlocks, sb.BlockSize, sb.InodeCount, sb.JournalBlocks)
}

func cmdMount(args []string) {
	fl := flag.NewFlagSet("mount", flag.ExitOnError)
	mode := fl.String("journal", "ordered", "journal mode: ordered | data | none")
	commit := fl.Duration("commit", 5*time.Second, "background commit interval (0 disables the timer)")
	cacheMB := fl.Int("cache-mb", 128, "buffer cache size in MiB")
	dirtyMB := fl.Int("dirty-mb", 32, "force a commit when this much file data is dirty")
	direct := fl.Bool("direct", false, "open the image with O_DIRECT")
	attr := fl.Duration("attr-timeout", time.Second, "kernel attribute/entry cache timeout")
	allow := fl.Bool("allow-other", false, "allow other users (needs user_allow_other in /etc/fuse.conf)")
	debug := fl.Bool("debug", false, "log every FUSE request")
	ro := fl.Bool("ro", false, "mount read-only")
	metricsAddr := fl.String("metrics", "", "serve Prometheus /metrics and /healthz on this address (e.g. :9100)")
	maxWrite := fl.Int("max-write", 1<<20, "maximum size of a single FUSE write")
	_ = fl.Parse(args)
	if fl.NArg() != 2 {
		usage()
	}
	mp, img := fl.Arg(0), fl.Arg(1)
	jm, err := engine.ParseMode(*mode)
	if err != nil {
		die("%v", err)
	}
	dev, err := blockdev.OpenFile(img, 0, *direct)
	if err != nil {
		die("opening image: %v", err)
	}
	e, info, err := engine.Open(dev, engine.Options{
		Mode: jm, CommitInterval: *commit, ReadOnly: *ro,
		CacheBlocks:      *cacheMB << 20 / format.BlockSize,
		DirtyLimitBlocks: *dirtyMB << 20 / format.BlockSize,
	})
	if err != nil {
		die("mount: %v", err)
	}
	if info.Journal.Txns > 0 || info.OrphansFreed > 0 {
		fmt.Fprintf(os.Stderr, "walfs: recovery: %s; %d orphan inode(s) reclaimed\n", info.Journal, info.OrphansFreed)
	}
	opts := []string{"default_permissions"}
	if *ro {
		opts = append(opts, "ro")
	}
	ops := &metrics.Ops{}
	if *metricsAddr != "" {
		go func() {
			if err := http.ListenAndServe(*metricsAddr, metrics.Handler(e, ops)); err != nil {
				fmt.Fprintf(os.Stderr, "walfs: metrics server: %v\n", err)
			}
		}()
		fmt.Fprintf(os.Stderr, "walfs: metrics on http://%s/metrics\n", *metricsAddr)
	}
	srv, err := fuse.NewServer(fusefs.NewRouter(fusefs.New(e, fusefs.Options{AttrTimeout: *attr, EntryTimeout: *attr}).WithMetrics(ops)), mp, &fuse.MountOptions{
		FsName: "walfs", Name: "walfs", AllowOther: *allow, Debug: *debug,
		MaxWrite: *maxWrite, Options: opts, MaxBackground: 64,
	})
	if err != nil {
		_ = e.Close()
		die("fuse mount: %v", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		if err := srv.Unmount(); err != nil {
			fmt.Fprintf(os.Stderr, "walfs: unmount: %v (is the filesystem busy?)\n", err)
		}
	}()
	fmt.Fprintf(os.Stderr, "walfs: mounted %s on %s (journal=%s, commit=%s)\n", img, mp, jm, *commit)
	srv.Serve()
	if err := e.Close(); err != nil {
		die("closing filesystem: %v", err)
	}
	s := e.Stats()
	fmt.Fprintf(os.Stderr, "walfs: clean unmount: %d commits, %d checkpoints, %d flushes\n", s.Commits, s.Checkpoints, s.Flushes)
	_ = dev.Close()
}

func cmdFsck(args []string) {
	fl := flag.NewFlagSet("fsck", flag.ExitOnError)
	replay := fl.Bool("replay", false, "replay a non-empty journal first (writes to the image)")
	_ = fl.Parse(args)
	if fl.NArg() != 1 {
		usage()
	}
	dev, err := blockdev.OpenFile(fl.Arg(0), 0, false)
	if err != nil {
		die("%v", err)
	}
	defer dev.Close()
	rep, err := fsck.Check(dev, fsck.Options{ReplayJournal: *replay})
	if err != nil {
		die("%v", err)
	}
	fmt.Println(rep)
	if !rep.OK() {
		os.Exit(1)
	}
}

func cmdInfo(args []string) {
	if len(args) != 1 {
		usage()
	}
	dev, err := blockdev.OpenFile(args[0], 0, false)
	if err != nil {
		die("%v", err)
	}
	defer dev.Close()
	b := make([]byte, format.BlockSize)
	if _, err := dev.ReadAt(b, 0); err != nil {
		die("%v", err)
	}
	sb, err := format.UnmarshalSuperblock(b)
	if err != nil {
		die("%v", err)
	}
	fmt.Printf("walfs version %d, %d blocks x %d bytes (%.1f MiB)\n", sb.Version, sb.TotalBlocks, sb.BlockSize,
		float64(sb.TotalBlocks*uint64(sb.BlockSize))/(1<<20))
	fmt.Printf("  uuid            %x\n", sb.UUID)
	fmt.Printf("  inodes          %d (table at block %d, %d blocks)\n", sb.InodeCount, sb.InodeTableStart, sb.InodeTableBlocks)
	fmt.Printf("  journal         blocks %d..%d (%d blocks)\n", sb.JournalStart, sb.JournalStart+sb.JournalBlocks-1, sb.JournalBlocks)
	fmt.Printf("  inode bitmap    block %d (%d blocks)\n", sb.InodeBitmapStart, sb.InodeBitmapBlocks)
	fmt.Printf("  block bitmap    block %d (%d blocks)\n", sb.BlockBitmapStart, sb.BlockBitmapBlocks)
	fmt.Printf("  data area       blocks %d..%d\n", sb.DataStart, sb.TotalBlocks-1)
	fmt.Printf("  orphan list     head inode %d\n", sb.OrphanHead)
	if j, err := format.ReadJournal(dev, sb); err == nil {
		txns, _, _ := j.Scan()
		fmt.Printf("  journal state   tail seq %d, %d committed transaction(s) awaiting replay\n", j.Super.TailSeq, len(txns))
	}
}

func cmdSnapshot(args []string) {
	if len(args) < 2 {
		usage()
	}
	action, img := args[0], args[1]
	dev, err := blockdev.OpenFile(img, 0, false)
	if err != nil {
		die("%v", err)
	}
	defer dev.Close()
	e, _, err := engine.Open(dev, engine.Options{Mode: engine.ModeOrdered})
	if err != nil {
		die("opening filesystem: %v", err)
	}
	switch action {
	case "list":
		for _, s := range e.ListSnapshots() {
			fmt.Printf("%-24s slot %-2d created %s\n", s.Name, s.Slot, time.Unix(0, s.CreatedNS).Format(time.RFC3339))
		}
	case "create", "delete":
		if len(args) != 3 {
			usage()
		}
		if action == "create" {
			err = e.CreateSnapshot(args[2])
		} else {
			err = e.DeleteSnapshot(args[2])
		}
		if err != nil {
			_ = e.Close()
			die("snapshot %s %q: %v", action, args[2], err)
		}
		fmt.Printf("snapshot %q %sd\n", args[2], action)
	default:
		usage()
	}
	if err := e.Close(); err != nil {
		die("closing filesystem: %v", err)
	}
}
