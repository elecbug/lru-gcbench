package bench

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

type ComparisonRow struct {
	GroupKey      string   `json:"group_key"`
	Backend       string   `json:"backend"`
	Case          string   `json:"case"`
	Runtime       string   `json:"runtime"`
	Phase         string   `json:"phase"`
	Metric        string   `json:"metric"`
	Baseline      float64  `json:"baseline_median"`
	Candidate     float64  `json:"candidate_median"`
	ChangePercent *float64 `json:"change_percent"`
}
type Comparison struct {
	BaselineLabel          string          `json:"baseline_label"`
	CandidateLabel         string          `json:"candidate_label"`
	BaselineWorkerSHA256   string          `json:"baseline_worker_sha256"`
	CandidateWorkerSHA256  string          `json:"candidate_worker_sha256"`
	BaselineHarnessSHA256  string          `json:"baseline_harness_sha256"`
	CandidateHarnessSHA256 string          `json:"candidate_harness_sha256"`
	HarnessOverride        bool            `json:"harness_override"`
	EnvironmentOverride    bool            `json:"environment_override"`
	UnmatchedGroups        int             `json:"unmatched_groups"`
	Rows                   []ComparisonRow `json:"rows"`
}

// CompareOptions records the independent exceptions explicitly requested by the caller.
// Neither exception permits different workload inputs.
type CompareOptions struct {
	AllowEnvironmentDiff bool
	AllowHarnessDiff     bool
}

// Compare retains the original API while keeping harness validation enabled.
func Compare(baseDir, candidateDir, out string, allowEnvDiff bool) error {
	return CompareWithOptions(baseDir, candidateDir, out, CompareOptions{AllowEnvironmentDiff: allowEnvDiff})
}

func compareProvenance(base, candidate Provenance, options CompareOptions) error {
	if base.Kind != candidate.Kind {
		return fmt.Errorf("cannot compare reference validation with actual upstream results")
	}
	missing := base.HarnessTreeSHA256 == "" || candidate.HarnessTreeSHA256 == ""
	legacyReference := base.Kind == "reference-validation-only" && base.HarnessTreeSHA256 == "" && candidate.HarnessTreeSHA256 == ""
	if !options.AllowHarnessDiff && ((missing && !legacyReference) || base.HarnessTreeSHA256 != candidate.HarnessTreeSHA256) {
		return fmt.Errorf("harness source hashes differ or are missing; rebuild both workers from the same harness or explicitly use -allow-harness-diff")
	}
	return nil
}

type phaseReplicate struct {
	group      string
	repeatSeed RepeatSeed
}

type workloadIdentity struct {
	Checksum, Operations, Reads, Writes uint64
}

func workloadIdentities(results []Result) (map[phaseReplicate]workloadIdentity, error) {
	identities := make(map[phaseReplicate]workloadIdentity)
	jobs := make(map[phaseReplicate]bool)
	for _, r := range results {
		replicate := RepeatSeed{Repeat: r.Job.Repeat, Seed: r.Job.Seed}
		jobKey := phaseReplicate{GroupKey(r.Job, ""), replicate}
		if jobs[jobKey] {
			return nil, fmt.Errorf("duplicate replicate in %s/%s/%s repeat=%d seed=%d", r.Job.Backend, r.Job.Case.Name, r.Job.Runtime.Name, replicate.Repeat, replicate.Seed)
		}
		jobs[jobKey] = true
		for _, p := range r.Phases {
			key := phaseReplicate{GroupKey(r.Job, p.Name), replicate}
			if _, exists := identities[key]; exists {
				return nil, fmt.Errorf("duplicate replicate phase %q in job %s", p.Name, r.Job.ID)
			}
			identities[key] = workloadIdentity{p.WorkloadChecksum, p.Operations, p.Reads, p.Writes}
		}
	}
	return identities, nil
}

func CompareWithOptions(baseDir, candidateDir, out string, options CompareOptions) error {
	bm, br, err := LoadSuite(baseDir)
	if err != nil {
		return err
	}
	cm, cr, err := LoadSuite(candidateDir)
	if err != nil {
		return err
	}
	if err = compareProvenance(bm.Capabilities.Provenance, cm.Capabilities.Provenance, options); err != nil {
		return err
	}
	bi, err := workloadIdentities(br)
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	ci, err := workloadIdentities(cr)
	if err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	ba, ca := AggregateResults(br), AggregateResults(cr)
	byKey := map[string]Aggregate{}
	for _, a := range ba {
		byKey[a.Key] = a
	}
	result := Comparison{BaselineLabel: bm.Label, CandidateLabel: cm.Label, BaselineWorkerSHA256: bm.WorkerSHA256, CandidateWorkerSHA256: cm.WorkerSHA256, EnvironmentOverride: options.AllowEnvironmentDiff, HarnessOverride: options.AllowHarnessDiff, BaselineHarnessSHA256: bm.Capabilities.Provenance.HarnessTreeSHA256, CandidateHarnessSHA256: cm.Capabilities.Provenance.HarnessTreeSHA256}
	matches := 0
	for _, c := range ca {
		b, ok := byKey[c.Key]
		if !ok {
			result.UnmatchedGroups++
			continue
		}
		matches++
		if !reflect.DeepEqual(b.Replicates, c.Replicates) {
			return fmt.Errorf("unequal successful repeat/seed sets in %s/%s/%s/%s; refusing a biased comparison", c.Backend, c.Case, c.Runtime, c.Phase)
		}
		for _, replicate := range b.Replicates {
			key := phaseReplicate{b.Key, replicate}
			if bi[key] != ci[key] {
				return fmt.Errorf("workload checksum or operation counts differ in %s/%s/%s/%s repeat=%d seed=%d; refusing comparison even with overrides", c.Backend, c.Case, c.Runtime, c.Phase, replicate.Repeat, replicate.Seed)
			}
		}
		if !options.AllowEnvironmentDiff && (b.MixedEnvironment || c.MixedEnvironment || b.EnvironmentKey != c.EnvironmentKey) {
			return fmt.Errorf("environment differs in %s; rerun on matching hosts/toolchains or explicitly use -allow-env-diff", c.Case)
		}
		var names []string
		for k := range c.Metrics {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			bv, ok := b.Metrics[k]
			if !ok {
				continue
			}
			cv := c.Metrics[k]
			// A metric missing in some replicates cannot support a paired comparison.
			if bv.N != cv.N || bv.N != len(b.Replicates) {
				continue
			}
			row := ComparisonRow{GroupKey: c.Key, Backend: c.Backend, Case: c.Case, Runtime: c.Runtime, Phase: c.Phase, Metric: k, Baseline: bv.Median, Candidate: cv.Median}
			if bv.Median != 0 {
				delta := (cv.Median/bv.Median - 1) * 100
				row.ChangePercent = &delta
			}
			result.Rows = append(result.Rows, row)
		}
	}
	result.UnmatchedGroups += len(ba) - matches
	if matches == 0 {
		return fmt.Errorf("no comparable groups: case/runtime settings must match exactly")
	}
	if err = reserveOutputDir(out); err != nil {
		return err
	}
	if err = WriteJSON(filepath.Join(out, "comparison.json"), result); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(out, "comparison.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"backend", "case", "runtime", "phase", "metric", "baseline_median", "candidate_median", "change_percent"})
	var md strings.Builder
	fmt.Fprintf(&md, "# Comparison: %s → %s\n\n", mdEscape(bm.Label), mdEscape(cm.Label))
	if bm.Capabilities.Provenance.Kind != "upstream-checkout" {
		md.WriteString("**REFERENCE VALIDATION ONLY — not google/go-lru performance data.**\n\n")
	}
	fmt.Fprintf(&md, "Matched groups: %d. Unmatched groups: %d. Environment override: %t. Harness override: %t.\n\n", matches, result.UnmatchedGroups, options.AllowEnvironmentDiff, options.AllowHarnessDiff)
	md.WriteString("Relative differences of medians only; not a statistical significance test. Negative is not universally better: throughput, hit rate and retained entry count need separate interpretation. A zero baseline has no percentage change.\n\n| Backend | Case | Runtime | Phase | Metric | Baseline | Candidate | Change % |\n|---|---|---|---|---|---:|---:|---:|\n")
	for _, v := range result.Rows {
		delta := ""
		if v.ChangePercent != nil {
			delta = number(*v.ChangePercent)
		}
		_ = w.Write([]string{v.Backend, v.Case, v.Runtime, v.Phase, v.Metric, number(v.Baseline), number(v.Candidate), delta})
		if v.Metric == "workload_ops_per_s" || v.Metric == "post_gc_heap_objects_bytes" || v.Metric == "gc_cpu_delta_seconds" || v.Metric == "hit_rate" {
			fmt.Fprintf(&md, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", mdEscape(v.Backend), mdEscape(v.Case), mdEscape(v.Runtime), mdEscape(v.Phase), v.Metric, number(v.Baseline), number(v.Candidate), delta)
		}
	}
	w.Flush()
	err = w.Error()
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return os.WriteFile(filepath.Join(out, "comparison.md"), []byte(md.String()), 0644)
}
