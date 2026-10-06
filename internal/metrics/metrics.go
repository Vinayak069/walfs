// Package metrics exposes walfs internals in the Prometheus text exposition
// format (no external dependencies), and counts FUSE operations.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/vinayak-singh/walfs/internal/engine"
)

// Ops counts filesystem requests; all fields are updated atomically.
type Ops struct {
	Reads, Writes, Fsyncs, Lookups, Creates, Unlinks, Renames atomic.Uint64
	ReadBytes, WrittenBytes                                   atomic.Uint64
	Errors                                                    atomic.Uint64
	ReadNanos, WriteNanos, FsyncNanos                         atomic.Uint64
}

func (o *Ops) Observe(c *atomic.Uint64, ns *atomic.Uint64, start time.Time) {
	c.Add(1)
	if ns != nil {
		ns.Add(uint64(time.Since(start)))
	}
}

func counter(w io.Writer, name, help string, v uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}

func gauge(w io.Writer, name, help string, v float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
}

// Write renders every metric.
func Write(w io.Writer, fs *engine.FS, ops *Ops) {
	st := fs.Stats()
	g := fs.Gauges()
	counter(w, "walfs_commits_total", "Journal transactions committed.", st.Commits)
	counter(w, "walfs_checkpoints_total", "Journal checkpoints performed.", st.Checkpoints)
	counter(w, "walfs_device_flushes_total", "Cache-flush barriers issued to the device.", st.Flushes)
	counter(w, "walfs_journal_blocks_written_total", "Blocks written to the journal.", st.JournalBlocks)
	counter(w, "walfs_checkpoint_blocks_written_total", "Metadata blocks written home at checkpoint.", st.HomeMetaBlocks)
	counter(w, "walfs_data_blocks_written_total", "Ordered-mode data blocks written at commit.", st.DataBlocks)
	counter(w, "walfs_revoked_blocks_total", "Revoke records written.", st.Revoked)

	fmt.Fprint(w, "# HELP walfs_commit_duration_seconds Latency of journal commits.\n# TYPE walfs_commit_duration_seconds histogram\n")
	cum := uint64(0)
	for i, b := range engine.CommitBounds {
		cum += st.CommitBuckets[i]
		fmt.Fprintf(w, "walfs_commit_duration_seconds_bucket{le=\"%g\"} %d\n", b.Seconds(), cum)
	}
	cum += st.CommitBuckets[len(engine.CommitBounds)]
	fmt.Fprintf(w, "walfs_commit_duration_seconds_bucket{le=\"+Inf\"} %d\n", cum)
	fmt.Fprintf(w, "walfs_commit_duration_seconds_sum %g\nwalfs_commit_duration_seconds_count %d\n",
		float64(st.CommitNanos)/1e9, cum)

	gauge(w, "walfs_free_blocks", "Free data blocks.", float64(g.FreeBlocks))
	gauge(w, "walfs_total_blocks", "Total data blocks.", float64(g.TotalBlocks))
	gauge(w, "walfs_free_inodes", "Free inodes.", float64(g.FreeInodes))
	gauge(w, "walfs_total_inodes", "Total inodes.", float64(g.TotalInodes))
	gauge(w, "walfs_journal_used_blocks", "Journal blocks holding unchecked-pointed transactions.", float64(g.JournalUsed))
	gauge(w, "walfs_journal_capacity_blocks", "Journal capacity in blocks.", float64(g.JournalCap))
	gauge(w, "walfs_dirty_data_blocks", "Ordered-mode data blocks awaiting commit.", float64(g.DirtyDataBlocks))
	gauge(w, "walfs_txn_blocks", "Blocks in the running transaction.", float64(g.TxnBlocks))
	gauge(w, "walfs_cache_blocks", "Blocks resident in the buffer cache.", float64(g.CacheBlocks))
	gauge(w, "walfs_pending_checkpoint_blocks", "Committed blocks awaiting their home write.", float64(g.PendingCheckpoint))
	ro := 0.0
	if g.ReadOnly {
		ro = 1
	}
	gauge(w, "walfs_read_only", "1 if the filesystem has gone read-only after an I/O error.", ro)

	if ops != nil {
		counter(w, "walfs_ops_read_total", "FUSE read requests.", ops.Reads.Load())
		counter(w, "walfs_ops_write_total", "FUSE write requests.", ops.Writes.Load())
		counter(w, "walfs_ops_fsync_total", "FUSE fsync requests.", ops.Fsyncs.Load())
		counter(w, "walfs_ops_lookup_total", "FUSE lookup requests.", ops.Lookups.Load())
		counter(w, "walfs_ops_create_total", "FUSE create/mkdir/mknod/symlink requests.", ops.Creates.Load())
		counter(w, "walfs_ops_unlink_total", "FUSE unlink/rmdir requests.", ops.Unlinks.Load())
		counter(w, "walfs_ops_rename_total", "FUSE rename requests.", ops.Renames.Load())
		counter(w, "walfs_read_bytes_total", "Bytes returned to readers.", ops.ReadBytes.Load())
		counter(w, "walfs_written_bytes_total", "Bytes accepted from writers.", ops.WrittenBytes.Load())
		counter(w, "walfs_errors_total", "FUSE requests that failed.", ops.Errors.Load())
		counter(w, "walfs_read_seconds_total", "Total time spent serving reads.", ops.ReadNanos.Load()/1e9)
		counter(w, "walfs_write_seconds_total", "Total time spent serving writes.", ops.WriteNanos.Load()/1e9)
		counter(w, "walfs_fsync_seconds_total", "Total time spent serving fsync.", ops.FsyncNanos.Load()/1e9)
	}
}

// Handler returns an http.Handler serving /metrics and a /healthz probe that
// fails once the filesystem has latched read-only because of an I/O error.
func Handler(fs *engine.FS, ops *Ops) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		Write(w, fs, ops)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := fs.Aborted(); err != nil {
			http.Error(w, "filesystem aborted: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return mux
}
