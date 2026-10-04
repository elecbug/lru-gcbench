package bench_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"example.com/lrugcbench/bench"
)

type cancelOnSecondJob struct {
	cancel context.CancelFunc
	jobs   int
}

func (w *cancelOnSecondJob) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "[") {
		w.jobs++
		if w.jobs == 2 {
			w.cancel()
		}
	}
	return len(p), nil
}

func TestPairedSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	worker := buildReference(t)
	cfg := integrationConfig()
	cfg.Cases = cfg.Cases[1:2]
	cfg.Cases[0].Capacity = 100
	cfg.Cases[0].KeySpace = 200
	cfg.Cases[0].Operations = 2000
	cfg.Cases[0].WarmupOps = 50
	cfg.Cases[0].ValueBytes = 16

	t.Run("fresh sequential workers and paired outputs", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "paired")
		m, err := bench.RunPaired(context.Background(), worker, worker, cfg, out, "A", "B", bench.CompareOptions{}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if m.FinishedAt == nil || len(m.Steps) != 4 {
			t.Fatalf("unfinished pair: %+v", m)
		}
		_, base, err := bench.LoadSuite(filepath.Join(out, "base"))
		if err != nil {
			t.Fatal(err)
		}
		_, candidate, err := bench.LoadSuite(filepath.Join(out, "candidate"))
		if err != nil {
			t.Fatal(err)
		}
		bySide := map[string][]bench.Result{"base": base, "candidate": candidate}
		pids := make(map[int]bool)
		var previousEnd time.Time
		for i, step := range m.Steps {
			if step.Status != "ok" {
				t.Fatalf("step not successful: %+v", step)
			}
			if i%2 == 1 && (step.Pair != m.Steps[i-1].Pair || step.Side == m.Steps[i-1].Side || step.JobID != m.Steps[i-1].JobID) {
				t.Fatal("non-adjacent or mismatched pair")
			}
			if i > 1 && i%2 == 0 && step.Side == m.Steps[i-2].Side {
				t.Fatal("starting side did not alternate")
			}
			r := bySide[step.Side][step.Pair]
			if pids[r.PID] || r.PID == os.Getpid() {
				t.Fatal("worker was not a fresh subprocess")
			}
			pids[r.PID] = true
			if r.StartedAt.Before(previousEnd) {
				t.Fatal("workers overlapped or ignored recorded order")
			}
			last := int64(0)
			for _, p := range r.Phases {
				last = max(last, p.End.Runtime.ElapsedNS)
			}
			previousEnd = r.StartedAt.Add(time.Duration(last))
		}
		for i := range base {
			if !reflect.DeepEqual(base[i].Job, candidate[i].Job) {
				t.Fatal("paired worker inputs differ")
			}
		}
		for _, name := range []string{"paired.json", "base/manifest.json", "candidate/manifest.json", "base/report.html", "candidate/report.html", "comparison/comparison.json"} {
			if st, err := os.Stat(filepath.Join(out, name)); err != nil || st.Size() == 0 {
				t.Fatalf("missing %s: %v", name, err)
			}
		}
		if _, err = bench.RunPaired(context.Background(), worker, worker, cfg, out, "A", "B", bench.CompareOptions{}, io.Discard); err == nil {
			t.Fatal("paired output overwritten")
		}
	})

	t.Run("failures preserved without comparison", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "paired")
		c := cfg
		c.Timeout = "1ns"
		m, err := bench.RunPaired(context.Background(), worker, worker, c, out, "A", "B", bench.CompareOptions{}, io.Discard)
		if err == nil || m.FinishedAt == nil || m.Error == "" {
			t.Fatal("paired failure not recorded")
		}
		for _, step := range m.Steps {
			if step.Status != "timeout" {
				t.Fatalf("expected timeout: %+v", step)
			}
		}
		for _, side := range []string{"base", "candidate"} {
			manifest, _, err := bench.LoadSuite(filepath.Join(out, side))
			if err != nil || manifest.FinishedAt == nil {
				t.Fatalf("missing final manifest: %v", err)
			}
			if _, err = os.Stat(filepath.Join(out, side, "report.md")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = os.Stat(filepath.Join(out, "comparison")); !os.IsNotExist(err) {
			t.Fatal("failed paired run produced comparison")
		}
	})

	t.Run("cancellation finalizes both manifests", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "paired")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		log := &cancelOnSecondJob{cancel: cancel}
		m, err := bench.RunPaired(ctx, worker, worker, cfg, out, "A", "B", bench.CompareOptions{}, log)
		if err == nil || m.FinishedAt == nil || m.Error == "" {
			t.Fatal("cancellation was not recorded")
		}
		if m.Steps[0].Status != "ok" {
			t.Fatal("completed worker result lost")
		}
		for _, step := range m.Steps[1:] {
			if step.Status != "cancelled" {
				t.Fatalf("cancellation not propagated: %+v", step)
			}
		}
		for _, side := range []string{"base", "candidate"} {
			manifest, _, err := bench.LoadSuite(filepath.Join(out, side))
			if err != nil || manifest.FinishedAt == nil {
				t.Fatalf("unfinalized %s: %v", side, err)
			}
			for _, job := range manifest.Jobs {
				if job.Status == "pending" {
					t.Fatal("cancelled job still pending")
				}
			}
		}
		if _, err = os.Stat(filepath.Join(out, "comparison")); !os.IsNotExist(err) {
			t.Fatal("cancelled pair produced comparison")
		}
	})
}
