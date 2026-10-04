package bench

import "time"

// Cache is implemented by a small typed adapter. Values never pass through any.
// Put includes creation of the configured payload. Get does not fill on a miss.
type Cache interface {
	Put(key string, id uint64) error
	Get(key string) bool
	Delete(key string) bool
	Compact()
	Stats() CacheStats
}

// PrefixCache is required only for delete_mode=prefix.
type PrefixCache interface {
	Cache
	DeletePrefix(prefix string)
}

type Factory func(backend string, c Case) (Cache, error)

type CacheStats struct {
	Entries                  int     `json:"entries"`
	CurrentSize              uint64  `json:"current_size"`
	MaxSize                  uint64  `json:"max_size"`
	GetHits                  uint64  `json:"get_hits"`
	GetMisses                uint64  `json:"get_misses"`
	EvictionsCapacity        uint64  `json:"evictions_capacity"`
	EvictionsPressure        uint64  `json:"evictions_pressure"`
	EvictionsDeleted         uint64  `json:"evictions_deleted"`
	CompactionsExplicit      uint64  `json:"compactions_explicit"`
	CompactionsPressureTier1 uint64  `json:"compactions_pressure_tier1"`
	CompactionsPressureTier2 uint64  `json:"compactions_pressure_tier2"`
	CompactionsAutoSlack     uint64  `json:"compactions_auto_slack"`
	MemoryPressure           float64 `json:"memory_pressure"`
	ArenaLiveNodes           int     `json:"arena_live_nodes"`
	ArenaFreeNodes           int     `json:"arena_free_nodes"`
	ArenaUnallocatedCap      int     `json:"arena_unallocated_cap"`
}

type Provenance struct {
	Kind              string `json:"kind"` // upstream-checkout or reference-validation-only
	TargetModule      string `json:"target_module"`
	TargetCommit      string `json:"target_commit,omitempty"`
	TargetDirty       bool   `json:"target_dirty"`
	TargetTreeSHA256  string `json:"target_tree_sha256"`
	HarnessTreeSHA256 string `json:"harness_tree_sha256,omitempty"`
	BuiltAt           string `json:"built_at"`
}
type Capabilities struct {
	SchemaVersion int        `json:"schema_version"`
	Backends      []string   `json:"backends"`
	Provenance    Provenance `json:"provenance"`
}
type Environment struct {
	GoVersion       string            `json:"go_version"`
	GOOS            string            `json:"goos"`
	GOARCH          string            `json:"goarch"`
	NumCPU          int               `json:"num_cpu"`
	GOMAXPROCS      int               `json:"gomaxprocs"`
	GOGC            int               `json:"gogc"`
	GOMEMLIMITBytes int64             `json:"gomemlimit_bytes"`
	Hostname        string            `json:"hostname"`
	CPUModel        string            `json:"cpu_model,omitempty"`
	Kernel          string            `json:"kernel,omitempty"`
	CgroupMemoryMax string            `json:"cgroup_memory_max,omitempty"`
	CgroupCPUmax    string            `json:"cgroup_cpu_max,omitempty"`
	CgroupCPUSet    string            `json:"cgroup_cpuset,omitempty"`
	GODEBUG         string            `json:"godebug"`
	GORACE          string            `json:"gorace"`
	BuildSettings   map[string]string `json:"build_settings"`
	MetricNames     []string          `json:"metric_names"`
}
type RuntimeSample struct {
	ElapsedNS           int64   `json:"elapsed_ns"`
	HeapObjectsBytes    uint64  `json:"heap_objects_bytes"`
	HeapLiveBytes       uint64  `json:"heap_live_bytes"`
	HeapGoalBytes       uint64  `json:"heap_goal_bytes"`
	HeapScanBytes       uint64  `json:"heap_scan_bytes"`
	RuntimeTotalBytes   uint64  `json:"runtime_total_bytes"`
	HeapReleasedBytes   uint64  `json:"heap_released_bytes"`
	HeapFreeBytes       uint64  `json:"heap_free_bytes"`
	RuntimeManagedBytes uint64  `json:"runtime_managed_bytes"` // total - released, NOT RSS.
	PressureActiveBytes uint64  `json:"pressure_active_bytes"` // total - released - heap/free.
	AllocBytes          uint64  `json:"alloc_bytes"`
	AllocObjects        uint64  `json:"alloc_objects"`
	GCCycles            uint64  `json:"gc_cycles"`
	GCForcedCycles      uint64  `json:"gc_forced_cycles"`
	GCCPUSeconds        float64 `json:"gc_cpu_seconds"`
	GCAssistCPUSeconds  float64 `json:"gc_assist_cpu_seconds"`
	AvailableCPUSeconds float64 `json:"available_cpu_seconds"`
	RSSBytes            uint64  `json:"rss_bytes"`
	RSSAvailable        bool    `json:"rss_available"`
}
type Snapshot struct {
	Runtime RuntimeSample `json:"runtime"`
	Cache   CacheStats    `json:"cache"`
}
type Histogram struct {
	// Boundaries are strings to preserve +/-Inf without invalid JSON numbers.
	BoundsSeconds []string `json:"bounds_seconds"`
	Counts        []uint64 `json:"counts"`
}
type QuantileInterval struct {
	LowerSeconds float64  `json:"lower_seconds"`
	UpperSeconds *float64 `json:"upper_seconds"` // null is an unbounded top bucket.
}
type LatencySummary struct {
	Samples   uint64            `json:"samples"`
	P50       *QuantileInterval `json:"p50,omitempty"`
	P95       *QuantileInterval `json:"p95,omitempty"`
	P99       *QuantileInterval `json:"p99,omitempty"`
	Histogram Histogram         `json:"histogram"`
}

// CompactionWindow marks one explicit Compact call within a concurrent workload.
// Times use the same origin as RuntimeSample.ElapsedNS. Scheduling determines
// whether individual requests overlap the call or wait for its cache lock.
type CompactionWindow struct {
	StartElapsedNS int64 `json:"start_elapsed_ns"`
	EndElapsedNS   int64 `json:"end_elapsed_ns"`
	DurationNS     int64 `json:"duration_ns"`
}

type Phase struct {
	ConcurrentCompaction *CompactionWindow `json:"concurrent_compaction,omitempty"`
	Name                 string            `json:"name"`
	DurationNS           int64             `json:"duration_ns"` // workload only, excludes boundary snapshots and explicit GC.
	Operations           uint64            `json:"operations"`
	Reads                uint64            `json:"reads"`
	Writes               uint64            `json:"writes"`
	Hits                 uint64            `json:"hits"`
	WorkloadChecksum     uint64            `json:"workload_checksum"`
	Start                Snapshot          `json:"start"`
	End                  Snapshot          `json:"end"`
	PostForcedGC         *Snapshot         `json:"post_forced_gc,omitempty"`
	GCPauses             Histogram         `json:"gc_pauses_delta"`
	GetLatency           LatencySummary    `json:"get_latency"`
	PutLatency           LatencySummary    `json:"put_latency"`
}
type Result struct {
	SchemaVersion  int             `json:"schema_version"`
	Job            Job             `json:"job"`
	PID            int             `json:"pid"`
	StartedAt      time.Time       `json:"started_at"`
	Provenance     Provenance      `json:"provenance"`
	Environment    Environment     `json:"environment"`
	Baseline       Snapshot        `json:"baseline"` // before cache construction; harness buffers already allocated.
	Phases         []Phase         `json:"phases"`
	Samples        []RuntimeSample `json:"samples"`
	DroppedSamples uint64          `json:"dropped_samples"`
	Warnings       []string        `json:"warnings"`
}
