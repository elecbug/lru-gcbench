// lrugcbench builds isolated workers, runs experiment matrices, and compares results.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"example.com/lrugcbench/bench"
	"example.com/lrugcbench/internal/buildworker"
)

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  lrugcbench build   -repo ../go-lru -source . -out bin/lru-worker
  lrugcbench run     -worker bin/lru-worker -config examples/smoke.json -out results/run1 -label baseline
  lrugcbench run-paired -base-worker bin/base -candidate-worker bin/candidate -config examples/smoke.json -out results/paired
  lrugcbench report  -dir results/run1
  lrugcbench compare -base results/run1 -candidate results/run2 -out results/diff

build uses an existing local checkout; it does not clone, fetch, or modify it.
run starts one fresh, sequential worker process per backend/runtime/case/repetition.
run-paired interleaves baseline and candidate jobs, alternating which side runs first.
Outputs are never silently overwritten. See README.md for methodology and caveats.`)
}
func execute(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage()
		return nil
	case "build":
		f := flag.NewFlagSet("build", flag.ContinueOnError)
		repo := f.String("repo", "", "local google/go-lru checkout (required)")
		source := f.String("source", ".", "lru-gcbench source directory")
		out := f.String("out", "", "new worker executable path (required)")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *repo == "" || *out == "" {
			return fmt.Errorf("build requires -repo and -out, with no positional arguments")
		}
		return buildworker.Build(ctx, *repo, *source, *out, os.Stderr)
	case "run":
		f := flag.NewFlagSet("run", flag.ContinueOnError)
		worker := f.String("worker", "", "worker executable (required)")
		config := f.String("config", "", "JSON experiment config (required)")
		out := f.String("out", "", "new result directory (required)")
		label := f.String("label", "run", "human-readable run label")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *worker == "" || *config == "" || *out == "" {
			return fmt.Errorf("run requires -worker, -config and -out")
		}
		c, err := bench.LoadConfig(*config)
		if err != nil {
			return err
		}
		_, err = bench.RunSuite(ctx, *worker, c, *out, *label, os.Stderr)
		return err
	case "run-paired":
		f := flag.NewFlagSet("run-paired", flag.ContinueOnError)
		baseWorker := f.String("base-worker", "", "baseline worker executable (required)")
		candidateWorker := f.String("candidate-worker", "", "candidate worker executable (required)")
		config := f.String("config", "", "shared JSON experiment config (required)")
		out := f.String("out", "", "new paired result directory (required)")
		baseLabel := f.String("base-label", "baseline", "baseline run label")
		candidateLabel := f.String("candidate-label", "candidate", "candidate run label")
		allowEnv := f.Bool("allow-env-diff", false, "explicitly allow differing hosts/toolchains/resource limits")
		allowHarness := f.Bool("allow-harness-diff", false, "explicitly allow differing or missing harness source hashes")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *baseWorker == "" || *candidateWorker == "" || *config == "" || *out == "" {
			return fmt.Errorf("run-paired requires -base-worker, -candidate-worker, -config and -out")
		}
		c, err := bench.LoadConfig(*config)
		if err != nil {
			return err
		}
		_, err = bench.RunPaired(ctx, *baseWorker, *candidateWorker, c, *out, *baseLabel, *candidateLabel, bench.CompareOptions{AllowEnvironmentDiff: *allowEnv, AllowHarnessDiff: *allowHarness}, os.Stderr)
		return err
	case "report":
		f := flag.NewFlagSet("report", flag.ContinueOnError)
		dir := f.String("dir", "", "existing run directory")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *dir == "" {
			return fmt.Errorf("report requires -dir")
		}
		return bench.Report(*dir)
	case "compare":
		f := flag.NewFlagSet("compare", flag.ContinueOnError)
		base := f.String("base", "", "baseline run directory")
		candidate := f.String("candidate", "", "candidate run directory")
		out := f.String("out", "", "new comparison directory")
		allow := f.Bool("allow-env-diff", false, "explicitly allow differing hosts/toolchains/resource limits")
		allowHarness := f.Bool("allow-harness-diff", false, "explicitly allow differing or missing harness source hashes")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *base == "" || *candidate == "" || *out == "" {
			return fmt.Errorf("compare requires -base, -candidate and -out")
		}
		return bench.CompareWithOptions(*base, *candidate, *out, bench.CompareOptions{AllowEnvironmentDiff: *allow, AllowHarnessDiff: *allowHarness})
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := execute(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
