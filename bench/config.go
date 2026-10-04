// Package bench implements a process-isolated cache memory/GC experiment protocol.
package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

const SchemaVersion = 1

type RuntimeConfig struct {
	Name       string `json:"name"`
	GOMAXPROCS int    `json:"gomaxprocs"`
	GOGC       int    `json:"gogc"`       // -1 disables the percentage-based trigger, not the memory limit.
	GOMEMLIMIT string `json:"gomemlimit"` // "off" or a Go-style byte limit.
}

type Case struct {
	CacheMode           string  `json:"cache_mode,omitempty"` // shared (default), independent per worker, or synthetic harness control.
	RetainSampleBuffer  bool    `json:"retain_sample_buffer,omitempty"`
	SampleCacheStats    bool    `json:"sample_cache_stats,omitempty"` // Opt-in observer; may contend for cache locks.
	Profile             string  `json:"profile,omitempty"`            // cpu, allocs, mutex, block; diagnostic runs only.
	ProfilePhase        string  `json:"profile_phase,omitempty"`
	ProfileRate         int     `json:"profile_rate,omitempty"`
	CompactMode         string  `json:"compact_mode,omitempty"`   // enabled (default) or disabled matched marker.
	CompactAt           float64 `json:"compact_at,omitempty"`     // Fraction of worker 0 stream, zero defaults to 0.5.
	CompactWindow       string  `json:"compact_window,omitempty"` // Fixed request-analysis width; empty defaults to 50ms.
	RequestTraceEvery   int     `json:"request_trace_every,omitempty"`
	MaxRequestSamples   int     `json:"max_request_samples,omitempty"`
	Name                string  `json:"name"`
	Scenario            string  `json:"scenario"` // footprint, steady, reclaim, concurrent-compact
	Capacity            int     `json:"capacity"`
	KeySpace            int     `json:"key_space"`
	KeyKind             string  `json:"key_kind"`   // flat or prefix; generated on demand, never retained by a corpus.
	ValueKind           string  `json:"value_kind"` // scalar (16 bytes, no pointers) or bytes
	ValueBytes          int     `json:"value_bytes"`
	PressureReclamation bool    `json:"pressure_reclamation"`
	Workers             int     `json:"workers"`
	WarmupOps           int     `json:"warmup_ops"`
	Operations          int     `json:"operations"`
	ReadPercent         int     `json:"read_percent"`
	DeleteMode          string  `json:"delete_mode,omitempty"` // Empty/keys: individual Delete; prefix: whole tenant groups.
	DeleteFraction      float64 `json:"delete_fraction"`
	SampleInterval      string  `json:"sample_interval"` // 0 disables periodic sampling.
	MaxSamples          int     `json:"max_samples"`
	LatencySampleEvery  int     `json:"latency_sample_every"` // 0 disables request timing.
}

type Config struct {
	SchemaVersion int             `json:"schema_version"`
	Repetitions   int             `json:"repetitions"`
	Seed          int64           `json:"seed"`
	Timeout       string          `json:"timeout"`
	Backends      []string        `json:"backends"`
	Runtimes      []RuntimeConfig `json:"runtimes"`
	Cases         []Case          `json:"cases"`
}

type Job struct {
	ID      string        `json:"id"`
	Backend string        `json:"backend"`
	Repeat  int           `json:"repeat"`
	Seed    int64         `json:"seed"`
	Runtime RuntimeConfig `json:"runtime"`
	Case    Case          `json:"case"`
}

func DecodeStrict(r io.Reader, v any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("more than one JSON document")
		}
		return err
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	// Decode defaults separately so explicit zero values retain their meaning.
	var raw struct {
		SchemaVersion int               `json:"schema_version"`
		Repetitions   int               `json:"repetitions"`
		Seed          int64             `json:"seed"`
		Timeout       string            `json:"timeout"`
		Backends      []string          `json:"backends"`
		Runtimes      []json.RawMessage `json:"runtimes"`
		Cases         []json.RawMessage `json:"cases"`
	}
	raw.SchemaVersion = 1
	raw.Repetitions = 3
	raw.Seed = 42
	raw.Timeout = "2m"
	if err := DecodeStrict(strings.NewReader(string(b)), &raw); err != nil {
		return c, err
	}
	c = Config{SchemaVersion: raw.SchemaVersion, Repetitions: raw.Repetitions, Seed: raw.Seed, Timeout: raw.Timeout, Backends: raw.Backends}
	for _, b := range raw.Runtimes {
		p := RuntimeConfig{GOMAXPROCS: 1, GOGC: 100, GOMEMLIMIT: "off"}
		if err := DecodeStrict(strings.NewReader(string(b)), &p); err != nil {
			return c, err
		}
		c.Runtimes = append(c.Runtimes, p)
	}
	for _, b := range raw.Cases {
		v := Case{Scenario: "steady", Capacity: 10000, KeyKind: "flat", ValueKind: "scalar", Workers: 1,
			WarmupOps: 10000, Operations: 200000, ReadPercent: 90, DeleteFraction: 0.75, SampleInterval: "50ms", MaxSamples: 2048}
		if err := DecodeStrict(strings.NewReader(string(b)), &v); err != nil {
			return c, err
		}
		if v.KeySpace == 0 {
			v.KeySpace = v.Capacity * 2
		}
		c.Cases = append(c.Cases, v)
	}
	return c, c.Validate()
}

func MemoryLimit(s string) (int64, error) {
	if s == "off" {
		return math.MaxInt64, nil
	}
	units := []struct {
		s string
		m int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(s, u.s) {
			n, err := strconv.ParseInt(strings.TrimSuffix(s, u.s), 10, 64)
			if err != nil || n <= 0 || n > math.MaxInt64/u.m {
				return 0, fmt.Errorf("invalid GOMEMLIMIT %q", s)
			}
			return n * u.m, nil
		}
	}
	return 0, fmt.Errorf("GOMEMLIMIT must be off or a positive integer with B/KiB/MiB/GiB/TiB: %q", s)
}

func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema_version %d", c.SchemaVersion)
	}
	if c.Repetitions < 1 || c.Repetitions > 1000 {
		return errors.New("repetitions must be in [1,1000]")
	}
	if t, err := time.ParseDuration(c.Timeout); err != nil || t <= 0 {
		return errors.New("timeout must be a positive duration")
	}
	if len(c.Backends) == 0 || len(c.Runtimes) == 0 || len(c.Cases) == 0 {
		return errors.New("backends, runtimes and cases must be nonempty")
	}
	seen := map[string]bool{}
	for _, b := range c.Backends {
		if b == "" || seen[b] {
			return fmt.Errorf("empty or duplicate backend %q", b)
		}
		seen[b] = true
	}
	seen = map[string]bool{}
	for _, p := range c.Runtimes {
		if p.Name == "" || seen[p.Name] {
			return fmt.Errorf("empty or duplicate runtime name %q", p.Name)
		}
		seen[p.Name] = true
		if p.GOMAXPROCS < 1 || p.GOMAXPROCS > 1024 || p.GOGC < -1 {
			return errors.New("gomaxprocs must be 1..1024 and gogc >= -1")
		}
		if _, err := MemoryLimit(p.GOMEMLIMIT); err != nil {
			return err
		}
	}
	seen = map[string]bool{}
	for _, v := range c.Cases {
		if v.Name == "" || seen[v.Name] {
			return fmt.Errorf("empty or duplicate case name %q", v.Name)
		}
		seen[v.Name] = true
		if err := v.Validate(); err != nil {
			return fmt.Errorf("case %s: %w", v.Name, err)
		}
	}
	count := int64(len(c.Cases)) * int64(len(c.Runtimes)) * int64(len(c.Backends)) * int64(c.Repetitions)
	if count > 10000 {
		return fmt.Errorf("%d jobs exceeds the 10000-job guardrail; split the config", count)
	}
	return nil
}

func (v Case) Validate() error {
	if v.Scenario != "footprint" && v.Scenario != "steady" && v.Scenario != "reclaim" && v.Scenario != "concurrent-compact" {
		return errors.New("scenario must be footprint, steady, reclaim or concurrent-compact")
	}
	if v.CacheMode != "" && v.CacheMode != "shared" && v.CacheMode != "independent" && v.CacheMode != "harness" {
		return errors.New("cache_mode must be shared, independent or harness")
	}
	if (v.CacheMode == "independent" || v.CacheMode == "harness") && v.Scenario != "steady" {
		return errors.New("independent/harness controls require the steady scenario")
	}
	if v.CacheMode == "harness" && v.PressureReclamation {
		return errors.New("harness control has no pressure reclamation")
	}
	if v.Profile == "" {
		if v.ProfilePhase != "" || v.ProfileRate != 0 {
			return errors.New("profile_phase/rate require profile")
		}
	} else {
		if v.Profile != "cpu" && v.Profile != "allocs" && v.Profile != "mutex" && v.Profile != "block" {
			return errors.New("profile must be cpu, allocs, mutex or block")
		}
		validPhase := v.ProfilePhase == "fill" || (v.Scenario == "steady" && (v.ProfilePhase == "measured" || v.ProfilePhase == "warmup" && v.WarmupOps > 0)) || (v.Scenario == "reclaim" && (v.ProfilePhase == "recovery" || v.ProfilePhase == "compact")) || (v.Scenario == "footprint" && v.ProfilePhase == "compact") || (v.Scenario == "concurrent-compact" && v.ProfilePhase == "concurrent")
		deletion := "delete"
		if v.DeleteMode == "prefix" {
			deletion = "delete_prefix"
		}
		validPhase = validPhase || v.Scenario != "steady" && v.ProfilePhase == deletion
		if !validPhase {
			return errors.New("profile_phase must identify an existing phase")
		}
		if v.ProfileRate < 0 || v.Profile == "cpu" && v.ProfileRate != 0 {
			return errors.New("profile_rate must be nonnegative; CPU profiling uses the runtime default")
		}
	}
	if v.CompactMode != "" && v.CompactMode != "enabled" && v.CompactMode != "disabled" {
		return errors.New("compact_mode must be enabled or disabled")
	}
	if math.IsNaN(v.CompactAt) || math.IsInf(v.CompactAt, 0) || v.CompactAt < 0 || v.CompactAt >= 1 {
		return errors.New("compact_at must be zero (default midpoint) or in (0,1)")
	}
	if v.CompactWindow != "" {
		if d, err := time.ParseDuration(v.CompactWindow); err != nil || d <= 0 || d > time.Hour {
			return errors.New("compact_window must be positive and at most 1h")
		}
	}
	if v.RequestTraceEvery < 0 || v.MaxRequestSamples < 0 || v.MaxRequestSamples > 1000000 {
		return errors.New("invalid request trace interval/capacity (maximum 1000000 samples)")
	}
	if v.Scenario != "concurrent-compact" && (v.CompactMode != "" || v.CompactAt != 0 || v.CompactWindow != "" || v.RequestTraceEvery != 0 || v.MaxRequestSamples != 0) {
		return errors.New("compact controls and request traces require concurrent-compact")
	}
	if v.RequestTraceEvery == 0 && v.MaxRequestSamples != 0 {
		return errors.New("max_request_samples requires request_trace_every")
	}
	if v.RequestTraceEvery > 0 {
		// Ceil per worker, summed. Reserve enough for the complete phase so a
		// short Compact cannot silently fall outside a truncated trace.
		if v.Workers < 1 || v.Workers > 1024 || v.Operations < 1 {
			return errors.New("request tracing requires valid workers and operations")
		}
		need := 0
		for w := 0; w < v.Workers; w++ {
			n := v.Operations / v.Workers
			if w < v.Operations%v.Workers {
				n++
			}
			need += n / v.RequestTraceEvery
			if n%v.RequestTraceEvery != 0 {
				need++
			}
		}
		if v.MaxRequestSamples < need {
			return fmt.Errorf("max_request_samples must be at least %d to retain the whole phase", need)
		}
	}
	if v.Capacity < 1 || v.Capacity > 100_000_000 || v.KeySpace < v.Capacity {
		return errors.New("capacity must be 1..100000000, key_space >= capacity")
	}
	if v.KeyKind != "flat" && v.KeyKind != "prefix" {
		return errors.New("key_kind must be flat or prefix")
	}
	if v.ValueKind != "scalar" && v.ValueKind != "bytes" {
		return errors.New("value_kind must be scalar or bytes")
	}
	if v.ValueBytes < 0 || v.ValueBytes > 1<<24 {
		return errors.New("value_bytes must be 0..16777216")
	}
	if v.ValueKind == "scalar" && v.ValueBytes != 0 {
		return errors.New("scalar is exactly 16 bytes; value_bytes must be 0")
	}
	if v.ValueKind == "bytes" && v.ValueBytes < 1 {
		return errors.New("bytes requires positive value_bytes")
	}
	if v.Workers < 1 || v.Workers > 1024 || v.WarmupOps < 0 || v.Operations < 1 {
		return errors.New("workers must be 1..1024, warmup_ops >= 0, operations > 0")
	}
	if v.ReadPercent < 0 || v.ReadPercent > 100 {
		return errors.New("read_percent must be 0..100")
	}
	if math.IsNaN(v.DeleteFraction) || v.DeleteFraction <= 0 || v.DeleteFraction >= 1 {
		return errors.New("delete_fraction must be in (0,1)")
	}
	if v.DeleteMode != "" && v.DeleteMode != "keys" && v.DeleteMode != "prefix" {
		return errors.New("delete_mode must be keys or prefix")
	}
	if v.DeleteMode == "prefix" {
		if v.Scenario == "steady" || v.KeyKind != "prefix" {
			return errors.New("prefix deletion requires prefix keys and a deletion scenario")
		}
		groups := v.DeleteFraction * 16
		if groups != math.Trunc(groups) {
			return errors.New("prefix delete_fraction must be a multiple of 1/16 (one tenant group)")
		}
	}
	if v.Scenario == "concurrent-compact" && (v.Operations/v.Workers < 2 || v.LatencySampleEvery == 0) {
		return errors.New("concurrent-compact requires operations >= 2*workers and latency_sample_every > 0")
	}
	sampleInterval, err := time.ParseDuration(v.SampleInterval)
	if err != nil || sampleInterval < 0 || (sampleInterval > 0 && sampleInterval < time.Millisecond) {
		return errors.New("sample_interval must be 0 or at least 1ms")
	}
	if v.SampleCacheStats && (sampleInterval == 0 || v.Scenario == "footprint") {
		return errors.New("sample_cache_stats requires periodic sampling in a non-footprint scenario")
	}
	if v.MaxSamples < 1 || v.MaxSamples > 100000 {
		return errors.New("max_samples must be 1..100000")
	}
	if v.LatencySampleEvery < 0 {
		return errors.New("latency_sample_every must be >= 0")
	}
	if v.Scenario == "footprint" && (v.Workers != 1 || v.PressureReclamation || v.LatencySampleEvery != 0) {
		return errors.New("footprint requires workers=1, pressure_reclamation=false and latency_sample_every=0")
	}
	return nil
}

func (c Config) Jobs() []Job {
	var jobs []Job
	for r := 0; r < c.Repetitions; r++ {
		for _, v := range c.Cases {
			for _, p := range c.Runtimes {
				for _, b := range c.Backends {
					jobs = append(jobs, Job{ID: fmt.Sprintf("job-%05d", len(jobs)+1), Backend: b, Repeat: r, Seed: c.Seed + int64(r), Runtime: p, Case: v})
				}
			}
		}
	}
	// Shuffle process order only; never use this PRNG for workload operations.
	rng := rand.New(rand.NewSource(c.Seed ^ 0x19d93ac1))
	rng.Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })
	return jobs
}

// GroupKey excludes repetition/seed and source identity, but includes EVERY case and runtime setting.
func GroupKey(j Job, phase string) string {
	b, _ := json.Marshal(struct {
		Backend string
		Runtime RuntimeConfig
		Case    Case
		Phase   string
	}{j.Backend, j.Runtime, j.Case, phase})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
