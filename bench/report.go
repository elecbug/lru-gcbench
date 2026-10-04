package bench

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Distribution struct {
	N        int             `json:"n"`
	Median   float64         `json:"median"`
	Min      float64         `json:"min"`
	Max      float64         `json:"max"`
	MedianCI *MedianInterval `json:"median_ci,omitempty"`
}
type RepeatSeed struct {
	Repeat int   `json:"repeat"`
	Seed   int64 `json:"seed"`
}
type Aggregate struct {
	Key              string                  `json:"key"`
	Backend          string                  `json:"backend"`
	Case             string                  `json:"case"`
	Runtime          string                  `json:"runtime"`
	Phase            string                  `json:"phase"`
	CacheMode        string                  `json:"cache_mode,omitempty"`
	Profile          string                  `json:"profile,omitempty"`
	Workers          int                     `json:"workers,omitempty"`
	EnvironmentKey   string                  `json:"environment_key"`
	MixedEnvironment bool                    `json:"mixed_environment"`
	Replicates       []RepeatSeed            `json:"replicates"`
	Metrics          map[string]Distribution `json:"metrics"`
	QualityWarnings  []string                `json:"quality_warnings,omitempty"`
}

func distribution(v []float64) Distribution {
	if len(v) == 0 {
		return Distribution{}
	}
	sort.Float64s(v)
	n := len(v)
	median := v[n/2]
	if n%2 == 0 {
		median = (v[n/2-1] + v[n/2]) / 2
	}
	return Distribution{N: n, Median: median, Min: v[0], Max: v[n-1], MedianCI: medianInterval(v)}
}
func EnvironmentKey(e Environment) string {
	// Exclude changing source VCS settings, but retain toolchain, build flags and resource caps.
	settings := map[string]string{}
	for k, v := range e.BuildSettings {
		if !strings.HasPrefix(k, "vcs") {
			settings[k] = v
		}
	}
	b, _ := json.Marshal(struct {
		Go, OS, Arch, Host, CPU, Kernel, Mem, Quota, CPUSet, Debug, Race string
		NCPU                                                             int
		Settings                                                         map[string]string
	}{
		e.GoVersion, e.GOOS, e.GOARCH, e.Hostname, e.CPUModel, e.Kernel, e.CgroupMemoryMax, e.CgroupCPUmax, e.CgroupCPUSet, e.GODEBUG, e.GORACE, e.NumCPU, settings})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func phaseMetrics(r Result, p Phase) map[string]float64 {
	a, b := p.Start.Runtime, p.End.Runtime
	out := map[string]float64{
		"duration_ms":            float64(p.DurationNS) / 1e6,
		"end_heap_objects_bytes": float64(b.HeapObjectsBytes), "end_heap_scan_bytes": float64(b.HeapScanBytes),
		"end_runtime_managed_bytes": float64(b.RuntimeManagedBytes), "end_pressure_active_bytes": float64(b.PressureActiveBytes),
		"allocated_bytes_delta": float64(sub(b.AllocBytes, a.AllocBytes)), "allocated_objects_delta": float64(sub(b.AllocObjects, a.AllocObjects)),
		"gc_cycles_delta": float64(sub(b.GCCycles, a.GCCycles)), "gc_forced_cycles_delta": float64(sub(b.GCForcedCycles, a.GCForcedCycles)),
		"gc_cpu_delta_seconds": math.Max(0, b.GCCPUSeconds-a.GCCPUSeconds), "gc_assist_delta_seconds": math.Max(0, b.GCAssistCPUSeconds-a.GCAssistCPUSeconds),
		"cache_entries_end": float64(p.End.Cache.Entries), "pressure_evictions_delta": float64(sub(p.End.Cache.EvictionsPressure, p.Start.Cache.EvictionsPressure)),
		"capacity_evictions_delta":         float64(sub(p.End.Cache.EvictionsCapacity, p.Start.Cache.EvictionsCapacity)),
		"pressure_compactions_delta":       float64(sub(p.End.Cache.CompactionsPressureTier1+p.End.Cache.CompactionsPressureTier2, p.Start.Cache.CompactionsPressureTier1+p.Start.Cache.CompactionsPressureTier2)),
		"pressure_tier1_compactions_delta": float64(sub(p.End.Cache.CompactionsPressureTier1, p.Start.Cache.CompactionsPressureTier1)),
		"pressure_tier2_compactions_delta": float64(sub(p.End.Cache.CompactionsPressureTier2, p.Start.Cache.CompactionsPressureTier2)),
		"explicit_compactions_delta":       float64(sub(p.End.Cache.CompactionsExplicit, p.Start.Cache.CompactionsExplicit)),
		"auto_slack_compactions_delta":     float64(sub(p.End.Cache.CompactionsAutoSlack, p.Start.Cache.CompactionsAutoSlack)),
	}
	if p.DurationNS > 0 {
		out["workload_ops_per_s"] = float64(p.Operations) * 1e9 / float64(p.DurationNS)
		if p.Reads > 0 {
			out["read_hits_per_s"] = float64(p.Hits) * 1e9 / float64(p.DurationNS)
			out["read_misses_per_s"] = float64(sub(p.Reads, p.Hits)) * 1e9 / float64(p.DurationNS)
		}
	}
	if r.Job.Backend == "arena" {
		out["arena_live_nodes_end"] = float64(p.End.Cache.ArenaLiveNodes)
		out["arena_free_nodes_end"] = float64(p.End.Cache.ArenaFreeNodes)
		out["arena_unallocated_capacity_end"] = float64(p.End.Cache.ArenaUnallocatedCap)
	}
	addNormalizedMetrics(out, r, p)
	if p.Reads > 0 {
		out["hit_rate"] = float64(p.Hits) / float64(p.Reads)
	}
	if p.PostForcedGC != nil {
		s := p.PostForcedGC.Runtime
		out["post_gc_heap_objects_bytes"] = float64(s.HeapObjectsBytes)
		out["post_gc_heap_scan_bytes"] = float64(s.HeapScanBytes)
		out["post_gc_heap_delta_from_baseline_bytes"] = float64(s.HeapObjectsBytes) - float64(r.Baseline.Runtime.HeapObjectsBytes)
	}
	if b.RSSAvailable {
		out["end_rss_bytes"] = float64(b.RSSBytes)
	}
	if denom := b.AvailableCPUSeconds - a.AvailableCPUSeconds; denom > 0 {
		out["gc_fraction_of_available_cpu"] = math.Max(0, b.GCCPUSeconds-a.GCCPUSeconds) / denom
	}
	if q := histogramQuantile(p.GCPauses, .99); q != nil {
		out["gc_pause_p99_lower_seconds"] = q.LowerSeconds
		if q.UpperSeconds != nil {
			out["gc_pause_p99_upper_seconds"] = *q.UpperSeconds
		}
	}
	if q := p.GetLatency.P99; q != nil {
		out["get_p99_lower_seconds"] = q.LowerSeconds
		if q.UpperSeconds != nil {
			out["get_p99_upper_seconds"] = *q.UpperSeconds
		}
	}
	if q := p.PutLatency.P99; q != nil {
		out["put_p99_lower_seconds"] = q.LowerSeconds
		if q.UpperSeconds != nil {
			out["put_p99_upper_seconds"] = *q.UpperSeconds
		}
	}
	// Periodic peaks are lower bounds on the true peak. Boundary observations included.
	peakHeap := max(a.HeapObjectsBytes, b.HeapObjectsBytes)
	peakRSS := max(a.RSSBytes, b.RSSBytes)
	rssOK := a.RSSAvailable || b.RSSAvailable
	for _, s := range r.Samples {
		if s.ElapsedNS >= a.ElapsedNS && s.ElapsedNS <= b.ElapsedNS {
			peakHeap = max(peakHeap, s.HeapObjectsBytes)
			if s.RSSAvailable {
				peakRSS = max(peakRSS, s.RSSBytes)
				rssOK = true
			}
		}
	}
	out["sampled_peak_heap_objects_bytes"] = float64(peakHeap)
	if rssOK {
		out["sampled_peak_rss_bytes"] = float64(peakRSS)
	}
	addCoverageMetrics(out, r, p)
	addWindowMetrics(out, p)
	if r.Job.Case.CacheMode == "harness" {
		// This control performs bookkeeping and payload construction without a
		// real cache. Synthetic outcomes cannot establish cache utility.
		for key := range out {
			if key == "hit_rate" || key == "read_hits_per_s" || key == "read_misses_per_s" || key == "cache_entries_end" || strings.HasPrefix(key, "arena_") || strings.Contains(key, "evictions") || strings.Contains(key, "compactions_delta") || key == "post_gc_heap_delta_per_entry_bytes" {
				delete(out, key)
			}
		}
	}
	return out
}
func AggregateResults(results []Result) []Aggregate {
	type group struct {
		a        Aggregate
		values   map[string][]float64
		warnings map[string]int
	}
	groups := map[string]*group{}
	for _, r := range results {
		for _, p := range r.Phases {
			key := GroupKey(r.Job, p.Name)
			g := groups[key]
			if g == nil {
				g = &group{a: Aggregate{Key: key, Backend: r.Job.Backend, Case: r.Job.Case.Name, Runtime: r.Job.Runtime.Name, Phase: p.Name, EnvironmentKey: EnvironmentKey(r.Environment), Metrics: map[string]Distribution{}}, values: map[string][]float64{}, warnings: map[string]int{}}
				g.a.CacheMode, g.a.Profile, g.a.Workers = r.Job.Case.CacheMode, r.Job.Case.Profile, r.Job.Case.Workers
				groups[key] = g
			}
			if g.a.EnvironmentKey != EnvironmentKey(r.Environment) {
				g.a.MixedEnvironment = true
			}
			g.a.Replicates = append(g.a.Replicates, RepeatSeed{Repeat: r.Job.Repeat, Seed: r.Job.Seed})
			for _, warning := range phaseQualityWarnings(r, p) {
				g.warnings[warning]++
			}
			for k, v := range phaseMetrics(r, p) {
				g.values[k] = append(g.values[k], v)
			}
		}
	}
	out := make([]Aggregate, 0, len(groups))
	for _, g := range groups {
		if g.a.MixedEnvironment {
			g.a.QualityWarnings = append(g.a.QualityWarnings, "Mixed worker environments; repeat distributions and median intervals may not describe a common population.")
		}
		if len(g.a.Replicates) < 10 {
			g.a.QualityWarnings = append(g.a.QualityWarnings, "Fewer than 10 independent worker repeats; repeat variability may be poorly characterized.")
		}
		for warning, count := range g.warnings {
			g.a.QualityWarnings = append(g.a.QualityWarnings, fmt.Sprintf("%d/%d runs: %s", count, len(g.a.Replicates), warning))
		}
		sort.Strings(g.a.QualityWarnings)
		for k, v := range g.values {
			g.a.Metrics[k] = distribution(v)
		}
		sort.Slice(g.a.Replicates, func(i, j int) bool {
			if g.a.Replicates[i].Repeat != g.a.Replicates[j].Repeat {
				return g.a.Replicates[i].Repeat < g.a.Replicates[j].Repeat
			}
			return g.a.Replicates[i].Seed < g.a.Replicates[j].Seed
		})
		out = append(out, g.a)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ak, bk := a.Backend+"/"+a.Case+"/"+a.Runtime, b.Backend+"/"+b.Case+"/"+b.Runtime
		if ak != bk {
			return ak < bk
		}
		order := map[string]int{"fill": 0, "warmup": 1, "measured": 2, "delete": 3, "delete_prefix": 3, "compact": 4, "concurrent": 4, "recovery": 5}
		ai, aok := order[a.Phase]
		bi, bok := order[b.Phase]
		if !aok {
			ai = 6
		}
		if !bok {
			bi = 6
		}
		if ai != bi {
			return ai < bi
		}
		return a.Phase < b.Phase
	})
	return out
}
func number(v float64) string { return strconv.FormatFloat(v, 'g', 10, 64) }
func medianCell(a Aggregate, k string, scale float64) string {
	v, ok := a.Metrics[k]
	if !ok {
		return "—"
	}
	return fmt.Sprintf("%.3f", v.Median/scale)
}
func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func Report(dir string) error {
	m, results, err := LoadSuite(dir)
	if err != nil {
		return err
	}
	groups := AggregateResults(results)
	if err = WriteJSON(filepath.Join(dir, "summary.json"), groups); err != nil {
		return err
	}
	var names []string
	set := map[string]bool{}
	for _, a := range groups {
		for k := range a.Metrics {
			set[k] = true
		}
	}
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	f, err := os.Create(filepath.Join(dir, "summary.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	header := []string{"group_key", "backend", "case", "runtime", "phase", "replicates", "cache_mode", "profile", "workers"}
	for _, k := range names {
		header = append(header, k+".median", k+".min", k+".max", k+".n", k+".median_ci.lower", k+".median_ci.upper", k+".median_ci.coverage")
	}
	_ = w.Write(header)
	for _, a := range groups {
		row := []string{a.Key, a.Backend, a.Case, a.Runtime, a.Phase, strconv.Itoa(len(a.Replicates)), a.CacheMode, a.Profile, strconv.Itoa(a.Workers)}
		for _, k := range names {
			v, ok := a.Metrics[k]
			if ok {
				row = append(row, number(v.Median), number(v.Min), number(v.Max), strconv.Itoa(v.N))
				if v.MedianCI != nil {
					row = append(row, number(v.MedianCI.Lower), number(v.MedianCI.Upper), number(v.MedianCI.Coverage))
				} else {
					row = append(row, "", "", "")
				}
			} else {
				row = append(row, "", "", "", "", "", "", "")
			}
		}
		_ = w.Write(row)
	}
	w.Flush()
	csvErr := w.Error()
	closeErr := f.Close()
	if csvErr != nil {
		return csvErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err = writeTraceCSV(filepath.Join(dir, "samples.csv"), results); err != nil {
		return err
	}
	if err = writeDiagnosticCSVs(dir, results); err != nil {
		return err
	}
	markdown := renderMarkdown(m, results, groups)
	if err = os.WriteFile(filepath.Join(dir, "report.md"), []byte(markdown), 0644); err != nil {
		return err
	}
	return writeHTMLReport(filepath.Join(dir, "report.html"), m, results, groups)
}
func writeTraceCSV(path string, results []Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"job_id", "backend", "case", "runtime", "repeat", "phase", "elapsed_seconds", "heap_objects_bytes", "heap_live_bytes", "heap_scan_bytes", "runtime_managed_bytes", "pressure_active_bytes", "rss_bytes", "gc_cycles", "gc_forced_cycles", "gc_cpu_seconds", "gc_assist_cpu_seconds", "cache_elapsed_seconds", "cache_entries", "pressure_evictions", "pressure_compactions"})
	for _, r := range results {
		for _, s := range r.Samples {
			phase := "boundary"
			for _, p := range r.Phases {
				if s.ElapsedNS >= p.Start.Runtime.ElapsedNS && s.ElapsedNS <= p.End.Runtime.ElapsedNS {
					phase = p.Name
					break
				}
			}
			rss := ""
			if s.RSSAvailable {
				rss = strconv.FormatUint(s.RSSBytes, 10)
			}
			cacheTime, entries, evictions, compactions := "", "", "", ""
			if s.CacheObserved && r.Job.Case.CacheMode != "harness" {
				cacheTime = number(float64(s.CacheElapsedNS) / 1e9)
				entries = strconv.Itoa(s.CacheEntries)
				evictions = strconv.FormatUint(s.PressureEvictions, 10)
				compactions = strconv.FormatUint(s.PressureCompactions, 10)
			}
			_ = w.Write([]string{r.Job.ID, r.Job.Backend, r.Job.Case.Name, r.Job.Runtime.Name, strconv.Itoa(r.Job.Repeat), phase, number(float64(s.ElapsedNS) / 1e9), strconv.FormatUint(s.HeapObjectsBytes, 10), strconv.FormatUint(s.HeapLiveBytes, 10), strconv.FormatUint(s.HeapScanBytes, 10), strconv.FormatUint(s.RuntimeManagedBytes, 10), strconv.FormatUint(s.PressureActiveBytes, 10), rss, strconv.FormatUint(s.GCCycles, 10), strconv.FormatUint(s.GCForcedCycles, 10), number(s.GCCPUSeconds), number(s.GCAssistCPUSeconds), cacheTime, entries, evictions, compactions})
		}
	}
	w.Flush()
	err = w.Error()
	ce := f.Close()
	if err != nil {
		return err
	}
	return ce
}
