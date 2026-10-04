package bench

import (
	"math/bits"
	"math/rand"
	"strconv"
	"sync"
	"time"
)

func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// MakeKey is injective on uint64 IDs. ASCII-only by design; no corpus retains keys.
// Flat: 16 hex bytes. Prefix: tenant/<hex group>/objects/<16 hex bytes>.
func MakeKey(id uint64, kind string) string {
	const hex = "0123456789abcdef"
	var b [48]byte
	n := 0
	if kind == "prefix" {
		n = copy(b[:], "tenant/")
		b[n] = hex[id&15]
		n++
		n += copy(b[n:], "/objects/")
	}
	x := mix64(id)
	for i := 15; i >= 0; i-- {
		b[n+i] = hex[x&15]
		x >>= 4
	}
	return string(b[:n+16])
}

// Eight buckets per factor-of-two interval, with exact nanosecond buckets below 8ns.
// Request quantiles are bucket intervals, never falsely advertised as exact p99.
const latencyBuckets = 496

type latencyHistogram [latencyBuckets]uint64

func (h *latencyHistogram) add(ns uint64) {
	i := int(ns)
	if ns >= 8 {
		e := bits.Len64(ns) - 1
		i = 8 + (e-3)*8 + int((ns>>uint(e-3))-8)
	}
	h[i]++
}
func latencyBound(i int) float64 {
	if i < 8 {
		return float64(i) * 1e-9
	}
	block := (i - 8) / 8
	offset := (i - 8) % 8
	return float64(uint64(8+offset)) * float64(uint64(1)<<uint(block)) * 1e-9
}
func (h *latencyHistogram) summary() LatencySummary {
	var total uint64
	for _, v := range h {
		total += v
	}
	if total == 0 {
		return LatencySummary{}
	}
	out := Histogram{Counts: make([]uint64, len(h)), BoundsSeconds: make([]string, len(h)+1)}
	copy(out.Counts, h[:])
	for i := range out.BoundsSeconds {
		out.BoundsSeconds[i] = strconv.FormatFloat(latencyBound(i), 'g', -1, 64)
	}
	return LatencySummary{Samples: total, P50: histogramQuantile(out, .5), P95: histogramQuantile(out, .95), P99: histogramQuantile(out, .99), Histogram: out}
}

type opCounts struct {
	operations, reads, writes, hits, checksum uint64
	get, put                                  latencyHistogram
}

func (o *opCounts) merge(v *opCounts) {
	o.operations += v.operations
	o.reads += v.reads
	o.writes += v.writes
	o.hits += v.hits
	o.checksum ^= v.checksum
	for i := range o.get {
		o.get[i] += v.get[i]
		o.put[i] += v.put[i]
	}
}

type mixedTask struct {
	start                          chan struct{}
	done                           chan struct{}
	results                        []opCounts
	errs                           []error
	compactionStart, compactionEnd time.Time
}

// Preparation happens before phase snapshots, so goroutine startup is not timed.
func prepareMixed(cache Cache, j Job, n int, stream uint64) *mixedTask {
	return prepareMixedTask(cache, j, n, stream, false)
}

func prepareMixedTask(cache Cache, j Job, n int, stream uint64, compact bool) *mixedTask {
	t := &mixedTask{start: make(chan struct{}), done: make(chan struct{}), results: make([]opCounts, j.Case.Workers), errs: make([]error, j.Case.Workers)}
	var triggerCompaction func()
	var compactDone chan struct{}
	if compact {
		trigger := make(chan struct{})
		started := make(chan struct{})
		ready := make(chan struct{})
		compactDone = make(chan struct{})
		var once sync.Once
		triggerCompaction = func() {
			once.Do(func() { close(trigger) })
			<-started
		}
		go func() {
			defer close(compactDone)
			close(ready)
			<-trigger
			t.compactionStart = time.Now()
			close(started)
			cache.Compact()
			t.compactionEnd = time.Now()
		}()
		<-ready
	}
	var ready, done sync.WaitGroup
	ready.Add(j.Case.Workers)
	done.Add(j.Case.Workers)
	for w := 0; w < j.Case.Workers; w++ {
		count := n / j.Case.Workers
		if w < n%j.Case.Workers {
			count++
		}
		go func(w, count int) {
			defer done.Done()
			compactAt := -1
			if compact && w == 0 {
				compactAt = count / 2
				// Also release the compactor on an early Put error.
				defer triggerCompaction()
			}
			seed := mix64(uint64(j.Seed) ^ uint64(w+1)*0x9e3779b97f4a7c15 ^ stream)
			rng := rand.New(rand.NewSource(int64(seed)))
			sampleEvery := j.Case.LatencySampleEvery
			nextSample := -1
			if sampleEvery > 0 {
				nextSample = int(mix64(seed^0x29f1c3) % uint64(sampleEvery))
			}
			out := &t.results[w]
			out.checksum = seed
			ready.Done()
			<-t.start
			for i := 0; i < count; i++ {
				if i == compactAt {
					triggerCompaction()
				}
				id := uint64(rng.Intn(j.Case.KeySpace))
				isRead := rng.Intn(100) < j.Case.ReadPercent
				key := MakeKey(id, j.Case.KeyKind)
				sampled := i == nextSample
				var then time.Time
				if sampled {
					then = time.Now()
					nextSample += sampleEvery
				}
				if isRead {
					if cache.Get(key) {
						out.hits++
					}
					out.reads++
				} else {
					if err := cache.Put(key, id); err != nil {
						t.errs[w] = err
						return
					}
					out.writes++
				}
				if sampled {
					ns := uint64(time.Since(then).Nanoseconds())
					if isRead {
						out.get.add(ns)
					} else {
						out.put.add(ns)
					}
				}
				tag := uint64(0)
				if isRead {
					tag = 1
				}
				out.checksum = bits.RotateLeft64(out.checksum, 7) ^ mix64(id^(tag<<63)^uint64(i))
				out.operations++
			}
		}(w, count)
	}
	ready.Wait()
	go func() {
		done.Wait()
		if compactDone != nil {
			<-compactDone
		}
		close(t.done)
	}()
	return t
}
func (t *mixedTask) run() (opCounts, error) {
	close(t.start)
	<-t.done
	var total opCounts
	for i := range t.results {
		if t.errs[i] != nil {
			return total, t.errs[i]
		}
		total.merge(&t.results[i])
	}
	return total, nil
}
