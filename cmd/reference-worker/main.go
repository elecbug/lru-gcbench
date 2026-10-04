// reference-worker exercises the harness ONLY. It cannot run google/go-lru backends.
package main

import (
	"example.com/lrugcbench/bench"
	"example.com/lrugcbench/internal/reference"
	"os"
)

func main() {
	p := bench.Provenance{Kind: "reference-validation-only", TargetModule: "internal/reference", TargetTreeSHA256: reference.SourceHash()}
	os.Exit(bench.WorkerMain(reference.Factory, bench.Capabilities{SchemaVersion: 1, Backends: []string{"reference"}, Provenance: p}))
}
