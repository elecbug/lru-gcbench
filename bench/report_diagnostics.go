package bench

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func windowMetricPrefix(name string) string {
	return "window_" + strings.ReplaceAll(name, "-", "_") + "_"
}

func addWindowMetrics(out map[string]float64, p Phase) {
	if probe := p.CompactProbe; probe != nil {
		out["compact_probe_entries_before"] = float64(probe.EntriesBefore)
		out["compact_probe_entries_after"] = float64(probe.EntriesAfter)
	}
	if len(p.RequestWindows) == 0 {
		return
	}
	out["request_trace_dropped"] = float64(p.RequestTraceDropped)
	for _, window := range p.RequestWindows {
		prefix := windowMetricPrefix(window.Name)
		out[prefix+"duration_ms"] = float64(window.EndElapsedNS-window.StartElapsedNS) / 1e6
		out[prefix+"samples"] = float64(window.Samples)
		out[prefix+"get_samples"] = float64(window.GetLatency.Samples)
		out[prefix+"put_samples"] = float64(window.PutLatency.Samples)
		if window.Name != "compact-overlap" && window.EndElapsedNS > window.StartElapsedNS && p.RequestTraceDropped == 0 {
			out[prefix+"estimated_ops_per_s"] = window.EstimatedOpsPerSecond
		}
		if q := window.GetLatency.P99; q != nil {
			out[prefix+"get_p99_lower_seconds"] = q.LowerSeconds
			if q.UpperSeconds != nil {
				out[prefix+"get_p99_upper_seconds"] = *q.UpperSeconds
			}
		}
		if q := window.PutLatency.P99; q != nil {
			out[prefix+"put_p99_lower_seconds"] = q.LowerSeconds
			if q.UpperSeconds != nil {
				out[prefix+"put_p99_upper_seconds"] = *q.UpperSeconds
			}
		}
	}
}

func writeDiagnosticCSVs(dir string, results []Result) error {
	if err := writeRequestTraceCSV(filepath.Join(dir, "request-trace.csv"), results); err != nil {
		return err
	}
	return writeCompactObservationsCSV(filepath.Join(dir, "compact-observations.csv"), results)
}

func finishCSV(f *os.File, w *csv.Writer) error {
	w.Flush()
	err, closeErr := w.Error(), f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func writeRequestTraceCSV(path string, results []Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"job_id", "backend", "case", "runtime", "repeat", "phase", "start_elapsed_ns", "end_elapsed_ns", "read", "hit"})
	for _, r := range results {
		for _, p := range r.Phases {
			for _, observation := range p.RequestTrace {
				_ = w.Write([]string{r.Job.ID, r.Job.Backend, r.Job.Case.Name, r.Job.Runtime.Name, strconv.Itoa(r.Job.Repeat), p.Name, strconv.FormatInt(observation.StartElapsedNS, 10), strconv.FormatInt(observation.EndElapsedNS, 10), strconv.FormatBool(observation.Read), strconv.FormatBool(observation.Hit)})
			}
		}
	}
	return finishCSV(f, w)
}

func writeCompactObservationsCSV(path string, results []Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"job_id", "backend", "case", "runtime", "repeat", "phase", "executed", "trigger_elapsed_ns", "end_elapsed_ns", "entries_before", "entries_after", "request_trace_dropped"})
	for _, r := range results {
		for _, p := range r.Phases {
			if probe := p.CompactProbe; probe != nil {
				_ = w.Write([]string{r.Job.ID, r.Job.Backend, r.Job.Case.Name, r.Job.Runtime.Name, strconv.Itoa(r.Job.Repeat), p.Name, strconv.FormatBool(probe.Executed), strconv.FormatInt(probe.TriggerElapsedNS, 10), strconv.FormatInt(probe.EndElapsedNS, 10), strconv.Itoa(probe.EntriesBefore), strconv.Itoa(probe.EntriesAfter), strconv.FormatUint(p.RequestTraceDropped, 10)})
			}
		}
	}
	return finishCSV(f, w)
}
