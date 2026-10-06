package crashtest

import (
	"fmt"
	"sort"
	"testing"

	"github.com/vinayak-singh/walfs/internal/engine"
	"github.com/vinayak-singh/walfs/internal/model"
)

// TestWorkloadCoverage reports how often each op kind succeeds / fails with
// each errno, to make sure the generator exercises success paths and the
// interesting error paths rather than mostly ENOENT.
func TestWorkloadCoverage(t *testing.T) {
	clk := newClock(1000)
	m := model.New(clk.now)
	r := &Runner{FS: ModelVFS{m}}
	g := NewGen(GenConfig{Seed: 7, Sparse: true})
	stat := map[string]int{}
	for i := 0; i < 20000; i++ {
		clk.add(10)
		op := g.Next(m)
		res := r.Apply(op)
		key := op.Kind + ":ok"
		if res.Err != nil {
			key = op.Kind + ":" + res.Err.Error()
		}
		stat[key]++
	}
	var keys []string
	for k := range stat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%-40s %d\n", k, stat[k])
	}
	_ = engine.Ctx{}
}
