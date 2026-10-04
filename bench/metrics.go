package bench

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"time"
)

var metricNames = []string{
	"/memory/classes/heap/objects:bytes", "/gc/heap/live:bytes", "/gc/heap/goal:bytes",
	"/gc/scan/heap:bytes", "/memory/classes/total:bytes", "/memory/classes/heap/released:bytes",
	"/memory/classes/heap/free:bytes", "/gc/heap/allocs:bytes", "/gc/heap/allocs:objects",
	"/gc/cycles/total:gc-cycles", "/gc/cycles/forced:gc-cycles", "/cpu/classes/gc/total:cpu-seconds",
	"/cpu/classes/gc/mark/assist:cpu-seconds", "/cpu/classes/total:cpu-seconds", "/sched/pauses/total/gc:seconds",
}

type collector struct {
	mu          sync.Mutex
	samples     []metrics.Sample
	origin      time.Time
	rss         *os.File
	rssBuf      [512]byte
	pageSize    uint64
	pauseBounds []string
}

func newCollector() (*collector, error) {
	supported := map[string]metrics.ValueKind{}
	for _, d := range metrics.All() {
		supported[d.Name] = d.Kind
	}
	c := &collector{origin: time.Now(), samples: make([]metrics.Sample, len(metricNames)), pageSize: uint64(os.Getpagesize())}
	for i, n := range metricNames {
		if _, ok := supported[n]; !ok {
			return nil, fmt.Errorf("required runtime metric unavailable: %s (Go %s)", n, runtime.Version())
		}
		c.samples[i].Name = n
	}
	if runtime.GOOS == "linux" {
		c.rss, _ = os.Open("/proc/self/statm")
	}
	metrics.Read(c.samples)
	h := c.samples[len(c.samples)-1].Value.Float64Histogram()
	c.pauseBounds = make([]string, len(h.Buckets))
	for i, b := range h.Buckets {
		c.pauseBounds[i] = strconv.FormatFloat(b, 'g', -1, 64)
	}
	return c, nil
}
func (c *collector) close() {
	if c.rss != nil {
		_ = c.rss.Close()
	}
}
func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// read never retains runtime-owned histogram storage. Periodic reads allocate no
// histogram copies and do not acquire the cache lock (important during Compact).
func (c *collector) read(pauses bool) (RuntimeSample, Histogram) {
	c.mu.Lock()
	defer c.mu.Unlock()
	metrics.Read(c.samples)
	s := c.samples
	r := RuntimeSample{ElapsedNS: time.Since(c.origin).Nanoseconds(),
		HeapObjectsBytes: s[0].Value.Uint64(), HeapLiveBytes: s[1].Value.Uint64(), HeapGoalBytes: s[2].Value.Uint64(),
		HeapScanBytes: s[3].Value.Uint64(), RuntimeTotalBytes: s[4].Value.Uint64(), HeapReleasedBytes: s[5].Value.Uint64(),
		HeapFreeBytes: s[6].Value.Uint64(), AllocBytes: s[7].Value.Uint64(), AllocObjects: s[8].Value.Uint64(),
		GCCycles: s[9].Value.Uint64(), GCForcedCycles: s[10].Value.Uint64(), GCCPUSeconds: s[11].Value.Float64(),
		GCAssistCPUSeconds: s[12].Value.Float64(), AvailableCPUSeconds: s[13].Value.Float64()}
	r.RuntimeManagedBytes = sub(r.RuntimeTotalBytes, r.HeapReleasedBytes)
	r.PressureActiveBytes = sub(r.RuntimeManagedBytes, r.HeapFreeBytes)
	if c.rss != nil {
		if _, err := c.rss.Seek(0, 0); err == nil {
			if n, err := c.rss.Read(c.rssBuf[:]); err == nil {
				// statm columns: size resident shared text lib data dt, measured in pages.
				data := c.rssBuf[:n]
				field := 0
				i := 0
				for i < len(data) {
					for i < len(data) && (data[i] == ' ' || data[i] == '\n') {
						i++
					}
					if i == len(data) {
						break
					}
					var val uint64
					digits := 0
					for i < len(data) && data[i] >= '0' && data[i] <= '9' {
						val = val*10 + uint64(data[i]-'0')
						i++
						digits++
					}
					if digits == 0 {
						break
					}
					if field == 1 {
						r.RSSBytes = val * c.pageSize
						r.RSSAvailable = true
						break
					}
					field++
				}
			}
		}
	}
	var h Histogram
	if pauses {
		v := s[len(s)-1].Value.Float64Histogram()
		h = Histogram{BoundsSeconds: c.pauseBounds, Counts: append([]uint64(nil), v.Counts...)}
	}
	return r, h
}

func histogramDelta(a, b Histogram) (Histogram, error) {
	if len(a.Counts) != len(b.Counts) || len(a.BoundsSeconds) != len(b.BoundsSeconds) {
		return Histogram{}, fmt.Errorf("GC histogram layout changed")
	}
	d := Histogram{BoundsSeconds: b.BoundsSeconds, Counts: make([]uint64, len(b.Counts))}
	for i := range a.BoundsSeconds {
		if a.BoundsSeconds[i] != b.BoundsSeconds[i] {
			return Histogram{}, fmt.Errorf("GC histogram boundary changed")
		}
	}
	for i := range a.Counts {
		if b.Counts[i] < a.Counts[i] {
			return Histogram{}, fmt.Errorf("GC histogram counter decreased")
		}
		d.Counts[i] = b.Counts[i] - a.Counts[i]
	}
	return d, nil
}
func histogramQuantile(h Histogram, q float64) *QuantileInterval {
	var total uint64
	for _, n := range h.Counts {
		total += n
	}
	if total == 0 {
		return nil
	}
	rank := uint64(math.Ceil(q * float64(total)))
	if rank == 0 {
		rank = 1
	}
	var n uint64
	for i, c := range h.Counts {
		n += c
		if n >= rank {
			lo, _ := strconv.ParseFloat(h.BoundsSeconds[i], 64)
			hi, _ := strconv.ParseFloat(h.BoundsSeconds[i+1], 64)
			if math.IsInf(lo, -1) {
				lo = 0
			}
			v := &QuantileInterval{LowerSeconds: lo}
			if !math.IsInf(hi, 1) {
				v.UpperSeconds = &hi
			}
			return v
		}
	}
	return nil
}

type tracer struct {
	points  []RuntimeSample
	used    int
	dropped uint64
	stop    chan struct{}
	done    chan struct{}
}

func newTracer(c *collector, interval time.Duration, maxSamples int, retainBuffer ...bool) *tracer {
	t := &tracer{}
	retain := len(retainBuffer) > 0 && retainBuffer[0]
	if interval == 0 && !retain {
		return t
	}
	// Allocate and touch the bounded buffer BEFORE the baseline GC. Retention
	// allows calibration-off to keep the same live buffer as calibration-on.
	t.points = make([]RuntimeSample, maxSamples)
	for i := range t.points {
		t.points[i].ElapsedNS = -1
	}
	if interval == 0 {
		return t
	}
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	return t
}
func (t *tracer) start(c *collector, interval time.Duration) {
	t.startWithCache(c, interval, nil)
}

// startWithCache opts into cache observations, which may acquire backend locks.
// Runtime-only sampling passes nil and never calls Stats. Cache observations
// have a separate completion timestamp because Stats may wait behind Compact.
func (t *tracer) startWithCache(c *collector, interval time.Duration, cache Cache) {
	if t.stop == nil {
		return
	}
	go func() {
		defer close(t.done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-tick.C:
				r, _ := c.read(false)
				if cache != nil {
					s := cache.Stats()
					r.CacheObserved = true
					r.CacheElapsedNS = time.Since(c.origin).Nanoseconds()
					r.CacheEntries = s.Entries
					r.PressureEvictions = s.EvictionsPressure
					r.PressureCompactions = s.CompactionsPressureTier1 + s.CompactionsPressureTier2
				}
				if t.used < len(t.points) {
					t.points[t.used] = r
					t.used++
				} else {
					t.dropped++
				}
			}
		}
	}()
}
func (t *tracer) finish() {
	if t.stop != nil {
		close(t.stop)
		<-t.done
	}
}

func environment(j Job) Environment {
	limit, _ := MemoryLimit(j.Runtime.GOMEMLIMIT)
	host, _ := os.Hostname()
	e := Environment{GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, NumCPU: runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0), GOGC: j.Runtime.GOGC, GOMEMLIMITBytes: limit, Hostname: host,
		GODEBUG: os.Getenv("GODEBUG"), GORACE: os.Getenv("GORACE"), BuildSettings: map[string]string{}, MetricNames: append([]string(nil), metricNames...)}
	if b, ok := debug.ReadBuildInfo(); ok {
		for _, s := range b.Settings {
			e.BuildSettings[s.Key] = s.Value
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }
	e.Kernel = read("/proc/sys/kernel/osrelease")
	e.CgroupMemoryMax = read("/sys/fs/cgroup/memory.max")
	e.CgroupCPUmax = read("/sys/fs/cgroup/cpu.max")
	e.CgroupCPUSet = read("/sys/fs/cgroup/cpuset.cpus.effective")
	b, _ := os.ReadFile("/proc/cpuinfo")
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if bytes.HasPrefix(line, []byte("model name")) {
			if x := bytes.IndexByte(line, ':'); x >= 0 {
				e.CPUModel = strings.TrimSpace(string(line[x+1:]))
			}
			break
		}
	}
	return e
}
