package bench_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"example.com/lrugcbench/bench"
)

func assertRunArtifacts(t *testing.T, out string, cfg bench.Config, parent string) {
	t.Helper()
	for _, name := range []string{"config.json", "command.json", "reproduce.sh", "run.log"} {
		st, err := os.Stat(filepath.Join(out, name))
		if err != nil || st.Size() == 0 {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	c, err := bench.LoadConfig(filepath.Join(out, "config.json"))
	if err != nil || !reflect.DeepEqual(c, cfg) {
		t.Fatalf("config changed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "command.json"))
	if err != nil {
		t.Fatal(err)
	}
	var command bench.RunCommand
	if err = json.Unmarshal(data, &command); err != nil || command.ParentPairedRun != parent {
		t.Fatalf("paired context: %v %+v", err, command)
	}
	if command.ReplayCommand[0] != "lrugcbench" || len(command.CLIArgs) != 0 {
		t.Fatal("library caller's test process mistaken for benchmark CLI")
	}
}

func TestFailedRunsPreserveReproductionArtifacts(t *testing.T) {
	cfg := integrationConfig()
	cfg.Cases = cfg.Cases[1:2]
	cfg.Repetitions = 1
	missing := filepath.Join(t.TempDir(), "missing-worker")
	t.Run("startup", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "startup")
		var progress bytes.Buffer
		m, err := bench.RunSuite(context.Background(), missing, cfg, out, "failure", &progress)
		if err == nil || m.Error == "" || m.FinishedAt == nil {
			t.Fatalf("startup error not recorded: %v %+v", err, m)
		}
		assertRunArtifacts(t, out, cfg, "")
		data, _ := os.ReadFile(filepath.Join(out, "run.log"))
		if !bytes.Equal(data, progress.Bytes()) || !strings.Contains(string(data), "Run failed:") {
			t.Fatal("failure log not preserved and mirrored")
		}
	})
	t.Run("paired startup", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "paired")
		m, err := bench.RunPaired(context.Background(), missing, missing, cfg, out, "A", "B", bench.CompareOptions{}, io.Discard)
		if err == nil || m.Error == "" || m.FinishedAt == nil {
			t.Fatalf("paired startup error not recorded: %v %+v", err, m)
		}
		assertRunArtifacts(t, out, cfg, "")
		assertRunArtifacts(t, filepath.Join(out, "base"), cfg, out)
		assertRunArtifacts(t, filepath.Join(out, "candidate"), cfg, out)
	})
	if testing.Short() {
		return
	}
	t.Run("timeout", func(t *testing.T) {
		worker := buildReference(t)
		out := filepath.Join(t.TempDir(), "timeout")
		cfg.Timeout = "1ns"
		m, err := bench.RunSuite(context.Background(), worker, cfg, out, "timeout", io.Discard)
		if err == nil || m.Jobs[0].Status != "timeout" {
			t.Fatalf("expected timeout: %v %+v", err, m)
		}
		assertRunArtifacts(t, out, cfg, "")
		data, _ := os.ReadFile(filepath.Join(out, "run.log"))
		if !strings.Contains(string(data), "timeout:") || !strings.Contains(string(data), "Run failed:") {
			t.Fatal("worker failure not logged")
		}
	})
}
