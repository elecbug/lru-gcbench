package bench

import (
	"fmt"
	"html/template"
	"os"
	"sort"
	"strings"
)

const qualityExplanation = "Quality thresholds are heuristics, not significance tests or guarantees: fewer than 10 repeats, request phases shorter than 1 second, fewer than 1000 Get/Put latency samples, and fewer than 100 GC pause events warrant investigation. Missing timing is not a zero latency."
const intervalExplanation = "Medians describe independent worker repeats. When at least 6 observations exist, median intervals use symmetric binomial order-statistic ranks with at least 95% coverage; the actual coverage is shown. Coverage assumes independent repeats from the same distribution. Intervals can be wide, do not establish a performance difference, and are omitted when too few observations exist."
const interpretation = "Throughput and allocations include key generation, PRNG, value construction and harness bookkeeping. Request quantiles are histogram bucket intervals around optionally timed adapter calls. GC CPU/op is reported only for natural-GC phases without forced cycles. Post-GC heap delta/entry subtracts the process baseline and divides by retained entries; it is omitted for nonpositive deltas or zero entries and is not exact cache-only memory. RSS is Linux process RSS; GOMEMLIMIT is not an RSS cap."
const samplingExplanation = "Sampled peaks are lower bounds and may miss brief transients. Periodic counts exclude boundary snapshots; the maximum gap includes both phase boundaries. Dropped counts apply to the whole job. Charts also include baseline, phase boundaries and post-GC snapshots. Dense charts retain bucket extrema and endpoints; raw samples.csv and raw/ retain the full recorded data. Cache stats are read at boundaries, not by the periodic sampler. Phase shading includes the boundary interval; duration metrics exclude snapshot overhead."

type reportColumn struct {
	Label, Key string
	Scale      float64
}
type reportTable struct {
	Title   string
	Headers []string
	Rows    [][]string
}

func metricTable(title string, groups []Aggregate, cols []reportColumn) reportTable {
	t := reportTable{Title: title, Headers: []string{"Backend", "Case", "Runtime", "Phase", "n"}}
	for _, c := range cols {
		t.Headers = append(t.Headers, c.Label)
	}
	for _, a := range groups {
		row := []string{a.Backend, a.Case, a.Runtime, a.Phase, fmt.Sprint(len(a.Replicates))}
		for _, c := range cols {
			row = append(row, medianCell(a, c.Key, c.Scale))
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

func reportTables(groups []Aggregate) []reportTable {
	return []reportTable{
		metricTable("Workload and memory", groups, []reportColumn{{"Duration ms", "duration_ms", 1}, {"Workload ops/s", "workload_ops_per_s", 1}, {"End heap MiB", "end_heap_objects_bytes", 1 << 20}, {"Post-GC heap MiB", "post_gc_heap_objects_bytes", 1 << 20}, {"GC CPU ms", "gc_cpu_delta_seconds", .001}, {"Hit rate", "hit_rate", 1}, {"Entries", "cache_entries_end", 1}}),
		metricTable("Normalized costs", groups, []reportColumn{{"B/op", "allocated_bytes_per_op", 1}, {"allocs/op", "allocated_objects_per_op", 1}, {"GC CPU ns/op", "gc_cpu_ns_per_op", 1}, {"Post-GC heap delta B/entry", "post_gc_heap_delta_per_entry_bytes", 1}, {"Concurrent Compact ms", "concurrent_compaction_duration_ms", 1}}),
		metricTable("Sampling coverage (median per run)", groups, []reportColumn{{"Get samples", "get_latency_samples", 1}, {"Put samples", "put_latency_samples", 1}, {"GC pause events", "gc_pause_events", 1}, {"Periodic samples", "periodic_sample_count", 1}, {"RSS samples", "periodic_rss_sample_count", 1}, {"Max gap ms", "periodic_max_gap_ms", 1}, {"Dropped/job", "job_dropped_samples", 1}}),
	}
}

func groupLabel(a Aggregate) string {
	return a.Backend + " / " + a.Case + " / " + a.Runtime + " / " + a.Phase
}

func recordedWarnings(results []Result) []string {
	set := map[string]bool{}
	for _, r := range results {
		for _, w := range r.Warnings {
			set[w] = true
		}
	}
	var warnings []string
	for w := range set {
		warnings = append(warnings, w)
	}
	sort.Strings(warnings)
	return warnings
}

func renderMarkdown(m Manifest, results []Result, groups []Aggregate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# LRU GC benchmark — %s\n\n", mdEscape(m.Label))
	if m.Capabilities.Provenance.Kind != "upstream-checkout" {
		b.WriteString("**REFERENCE VALIDATION ONLY. These are NOT measurements of google/go-lru.**\n\n")
	}
	fmt.Fprintf(&b, "Successful jobs: **%d / %d**. Worker SHA256: `%s`.\n\nTarget commit: `%s`; dirty: `%t`; source SHA256: `%s`.\n\n", len(results), len(m.Jobs), m.WorkerSHA256, m.Capabilities.Provenance.TargetCommit, m.Capabilities.Provenance.TargetDirty, m.Capabilities.Provenance.TargetTreeSHA256)
	fmt.Fprintf(&b, "%s\n\nFull distributions and median intervals are in summary.json, summary.csv and the offline report.html. Missing metrics appear as —.\n\n", intervalExplanation)
	for _, t := range reportTables(groups) {
		fmt.Fprintf(&b, "## %s\n\n", t.Title)
		b.WriteString("| " + strings.Join(t.Headers, " | ") + " |\n|")
		for range t.Headers {
			b.WriteString("---|")
		}
		b.WriteString("\n")
		for _, row := range t.Rows {
			b.WriteString("|")
			for _, cell := range row {
				b.WriteString(" " + mdEscape(cell) + " |")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "## Quality warnings\n\n%s\n\n", qualityExplanation)
	warned := false
	for _, a := range groups {
		for _, warning := range a.QualityWarnings {
			fmt.Fprintf(&b, "- %s: %s\n", mdEscape(groupLabel(a)), mdEscape(warning))
			warned = true
		}
	}
	if !warned {
		b.WriteString("No heuristic warnings were triggered. This does not establish statistical significance.\n")
	}
	fmt.Fprintf(&b, "\n## Interpretation\n\n%s\n\n%s\n\nFootprint forces GC after retained-memory phases; natural-GC phases exclude explicit post-phase collections. The concurrent phase runs one Compact while the mixed request workload is active; scheduling can serialize calls, so individual requests are not guaranteed to overlap Compact.\n", interpretation, samplingExplanation)
	for _, rec := range m.Jobs {
		if rec.Status != "ok" {
			fmt.Fprintf(&b, "\n- Unsuccessful / pending job %s: %s — %s\n", mdEscape(rec.Job.ID), mdEscape(rec.Status), mdEscape(rec.Error))
		}
	}
	if warnings := recordedWarnings(results); len(warnings) > 0 {
		b.WriteString("\n## Recorded caveats\n\n")
		for _, warning := range warnings {
			fmt.Fprintf(&b, "- %s\n", mdEscape(warning))
		}
	}
	return b.String()
}

type metricRow struct {
	Name         string
	Distribution Distribution
}
type detailGroup struct {
	Label    string
	Warnings []string
	Metrics  []metricRow
}
type htmlReport struct {
	Manifest                                     Manifest
	Successful                                   int
	Reference                                    bool
	Tables                                       []reportTable
	Groups                                       []detailGroup
	Charts                                       []memoryChart
	Warnings                                     []string
	Problems                                     []JobRecord
	Quality, Intervals, Interpretation, Sampling string
}

func writeHTMLReport(path string, m Manifest, results []Result, groups []Aggregate) error {
	data := htmlReport{Manifest: m, Successful: len(results), Reference: m.Capabilities.Provenance.Kind != "upstream-checkout", Tables: reportTables(groups), Warnings: recordedWarnings(results), Quality: qualityExplanation, Intervals: intervalExplanation, Interpretation: interpretation, Sampling: samplingExplanation}
	for _, a := range groups {
		g := detailGroup{Label: groupLabel(a), Warnings: a.QualityWarnings}
		for k, v := range a.Metrics {
			g.Metrics = append(g.Metrics, metricRow{Name: k, Distribution: v})
		}
		sort.Slice(g.Metrics, func(i, j int) bool { return g.Metrics[i].Name < g.Metrics[j].Name })
		data.Groups = append(data.Groups, g)
	}
	for _, r := range results {
		data.Charts = append(data.Charts, makeMemoryChart(r))
	}
	for _, rec := range m.Jobs {
		if rec.Status != "ok" {
			data.Problems = append(data.Problems, rec)
		}
	}
	t, err := template.New("report").Funcs(template.FuncMap{"num": number, "pct": func(v float64) string { return fmt.Sprintf("%.3f%%", 100*v) }}).Parse(htmlReportTemplate)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	err = t.Execute(f, data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

const htmlReportTemplate = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>LRU GC benchmark — {{.Manifest.Label}}</title>
<style>body{font:15px/1.6 system-ui;margin:32px auto;max-width:1500px;padding:0 24px;color:#172333;background:#fff}h1{font-size:28px}h2{margin-top:2em}a{color:#135b96}table{border-collapse:collapse;width:100%;font-variant-numeric:tabular-nums;font-size:13px}td,th{padding:7px 10px;border-bottom:1px solid #dce3e9;text-align:right;white-space:nowrap}th{background:#eef3f7}td:first-child,th:first-child{text-align:left}.scroll{overflow:auto;margin:16px 0}.warning{background:#fff4d6;border-left:4px solid #b47800;padding:12px 18px}details{border:1px solid #dce3e9;border-radius:6px;margin:12px 0;padding:10px 16px}summary{cursor:pointer;font-weight:600;overflow-wrap:anywhere}code{overflow-wrap:anywhere}svg{display:block;width:100%;min-width:600px;height:auto}.chart{overflow-x:auto}.heap{fill:none;stroke:#1267b1;stroke-width:2}.rss{fill:none;stroke:#b74926;stroke-width:2;stroke-dasharray:5 3}.axis{stroke:#9baab8;stroke-width:1}.phase{fill:#537d99;fill-opacity:.09}.phase:nth-of-type(even){fill-opacity:.17}.compact{fill:#c1841c;fill-opacity:.22}svg text{font-size:11px;fill:#354555}.legend span{display:inline-block;margin-right:20px}.legend .heap-label{color:#1267b1}.legend .rss-label{color:#b74926}p{max-width:1100px}.muted{color:#536373}</style></head><body>
<h1>LRU GC benchmark — {{.Manifest.Label}}</h1>
{{if .Reference}}<p class="warning"><strong>REFERENCE VALIDATION ONLY. These are NOT measurements of google/go-lru.</strong></p>{{end}}
<p>Successful jobs: <strong>{{.Successful}} / {{len .Manifest.Jobs}}</strong>. <a href="summary.json">Summary JSON</a> · <a href="summary.csv">Summary CSV</a> · <a href="samples.csv">Time-series CSV</a> · <a href="manifest.json">Manifest</a> · <a href="report.md">Markdown</a></p>
<p>Worker SHA256: <code>{{.Manifest.WorkerSHA256}}</code><br>Target commit: <code>{{.Manifest.Capabilities.Provenance.TargetCommit}}</code>; dirty: {{.Manifest.Capabilities.Provenance.TargetDirty}}; source SHA256: <code>{{.Manifest.Capabilities.Provenance.TargetTreeSHA256}}</code></p>
<p>{{.Intervals}} Missing metrics appear as —.</p>
{{range .Tables}}<h2>{{.Title}}</h2><div class="scroll"><table><thead><tr>{{range .Headers}}<th scope="col">{{.}}</th>{{end}}</tr></thead><tbody>{{range .Rows}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>{{end}}</tbody></table></div>{{end}}
<h2>Quality and full distributions</h2><p>{{.Quality}}</p>
{{range .Groups}}<details><summary>{{.Label}} — {{len .Warnings}} quality warnings</summary>{{if .Warnings}}<ul class="warning">{{range .Warnings}}<li>{{.}}</li>{{end}}</ul>{{else}}<p>No heuristic warnings triggered; this does not establish significance.</p>{{end}}
<div class="scroll"><table><thead><tr><th>Metric</th><th>n</th><th>Median</th><th>Min</th><th>Max</th><th>Median interval</th><th>Coverage</th></tr></thead><tbody>{{range .Metrics}}<tr><td>{{.Name}}</td><td>{{.Distribution.N}}</td><td>{{num .Distribution.Median}}</td><td>{{num .Distribution.Min}}</td><td>{{num .Distribution.Max}}</td>{{with .Distribution.MedianCI}}<td>[{{num .Lower}}, {{num .Upper}}]</td><td>{{pct .Coverage}}</td>{{else}}<td>—</td><td>—</td>{{end}}</tr>{{end}}</tbody></table></div></details>{{end}}
<h2>Memory traces by job</h2><p>{{.Sampling}}</p><p class="legend"><span class="heap-label">Solid blue: heap objects</span><span class="rss-label">Dashed orange: RSS (where available)</span><span>Shading: phases; amber: concurrent Compact</span></p>
{{range .Charts}}<details open><summary>{{.Label}}</summary><p class="muted">{{.Plotted}} / {{.Observations}} observations plotted, including snapshots; {{.Periodic}} retained periodic samples; {{.Dropped}} dropped samples in job.</p>
<div class="chart"><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 960 292" role="img" aria-label="Heap and RSS timeline for {{.Label}}">
{{range .Phases}}<rect class="phase" x="{{.X}}" y="24" width="{{.Width}}" height="176"><title>{{.Name}}: {{.Start}}–{{.End}} seconds</title></rect><text x="{{.X}}" y="{{.LabelY}}">{{.Name}}</text>{{end}}
{{range .Events}}<rect class="compact" x="{{.X}}" y="24" width="{{.Width}}" height="176"><title>{{.Name}}: {{.Start}}–{{.End}} seconds</title></rect>{{end}}
<line class="axis" x1="60" y1="200" x2="940" y2="200"/><line class="axis" x1="60" y1="24" x2="60" y2="200"/>
{{range .Ticks}}<text x="{{.X}}" y="217" text-anchor="middle">{{.Label}}</text>{{end}}
<text x="54" y="29" text-anchor="end">{{.MaxMiB}}</text><text x="54" y="202" text-anchor="end">0</text><text x="10" y="15">MiB</text><text x="940" y="282" text-anchor="end">elapsed seconds</text>
<path class="heap" d="{{.HeapPath}}"/><path class="rss" d="{{.RSSPath}}"/></svg></div>
<div class="scroll"><table><thead><tr><th>Phase / event</th><th>Start s</th><th>End s</th></tr></thead><tbody>{{range .Phases}}<tr><td>{{.Name}}</td><td>{{.Start}}</td><td>{{.End}}</td></tr>{{end}}{{range .Events}}<tr><td>{{.Name}}</td><td>{{.Start}}</td><td>{{.End}}</td></tr>{{end}}</tbody></table></div></details>{{end}}
<h2>Interpretation</h2><p>{{.Interpretation}}</p><p>Footprint forces GC after retained-memory phases. Natural-GC counters and pause histograms exclude explicit post-phase collections. A concurrent phase runs one Compact with the mixed request workload active; scheduling can serialize calls, and individual requests are not guaranteed to overlap Compact.</p>
{{if .Problems}}<h2>Unsuccessful / pending jobs</h2><ul>{{range .Problems}}<li>{{.Job.ID}}: {{.Status}} — {{.Error}}</li>{{end}}</ul>{{end}}
{{if .Warnings}}<h2>Recorded caveats</h2><ul>{{range .Warnings}}<li>{{.}}</li>{{end}}</ul>{{end}}</body></html>`
