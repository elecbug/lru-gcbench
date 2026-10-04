package bench

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type topologyCache struct {
	*recordingCache
	compactions int
}

func (c *topologyCache) Compact() { c.compactions++ }
func (c *topologyCache) DeletePrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.items {
		if strings.HasPrefix(key, prefix) {
			delete(c.items, key)
		}
	}
}

func TestIndependentCachesRouteAndBroadcast(t *testing.T) {
	j := Job{Backend: "test", Case: testCase()}
	j.Case.CacheMode, j.Case.Workers, j.Case.DeleteMode = "independent", 3, "prefix"
	var made []*topologyCache
	factory := func(backend string, c Case) (Cache, error) {
		if backend != j.Backend || !reflect.DeepEqual(c, j.Case) {
			t.Fatalf("factory settings changed: %s %+v", backend, c)
		}
		cache := &topologyCache{recordingCache: newRecordingCache()}
		made = append(made, cache)
		return cache, nil
	}
	cache, err := newCacheForJob(j, factory)
	if err != nil {
		t.Fatal(err)
	}
	if len(made) != 3 || cacheMultiplicity(j.Case) != 3 {
		t.Fatal("independent mode did not construct one full cache per worker")
	}
	if err := cache.Put("tenant/0/common", 1); err != nil {
		t.Fatal(err)
	}
	group := cache.(workerCaches)
	for w, child := range made {
		if group.ForWorker(w) != child || !child.Get("tenant/0/common") {
			t.Fatal("fill was not broadcast or worker was routed incorrectly")
		}
	}
	if err := group.ForWorker(0).Put("private", 2); err != nil {
		t.Fatal(err)
	}
	if group.ForWorker(1).Get("private") || group.ForWorker(2).Get("private") {
		t.Fatal("independent caches shared a request write")
	}
	if s := cache.Stats(); s.Entries != 4 || s.GetHits != 3 || s.GetMisses != 2 {
		t.Fatalf("group counters not aggregated: %+v", s)
	}
	cache.(PrefixCache).DeletePrefix("tenant/0/")
	if cache.Stats().Entries != 1 || !cache.Delete("private") || cache.Stats().Entries != 0 {
		t.Fatal("group deletion failed")
	}
	cache.Compact()
	for _, child := range made {
		if child.compactions != 1 {
			t.Fatal("Compact was not broadcast")
		}
	}
}

type fixedStatsCache struct {
	Cache
	stats CacheStats
}

func (c fixedStatsCache) Stats() CacheStats { return c.stats }

func TestGroupStatsSumCountersAndMaxPressure(t *testing.T) {
	a := CacheStats{Entries: 2, CurrentSize: 2, MaxSize: 9, GetHits: 3, GetMisses: 4,
		EvictionsCapacity: 5, EvictionsPressure: 6, EvictionsDeleted: 7,
		CompactionsExplicit: 8, CompactionsPressureTier1: 9, CompactionsPressureTier2: 10,
		CompactionsAutoSlack: 11, MemoryPressure: .6, ArenaLiveNodes: 12, ArenaFreeNodes: 13, ArenaUnallocatedCap: 14}
	b := a
	b.MemoryPressure = .9
	g := cacheGroup{caches: []Cache{fixedStatsCache{stats: a}, fixedStatsCache{stats: b}}}
	want := CacheStats{Entries: 4, CurrentSize: 4, MaxSize: 18, GetHits: 6, GetMisses: 8,
		EvictionsCapacity: 10, EvictionsPressure: 12, EvictionsDeleted: 14,
		CompactionsExplicit: 16, CompactionsPressureTier1: 18, CompactionsPressureTier2: 20,
		CompactionsAutoSlack: 22, MemoryPressure: .9, ArenaLiveNodes: 24, ArenaFreeNodes: 26, ArenaUnallocatedCap: 28}
	if got := g.Stats(); got != want {
		t.Fatalf("Stats = %+v, want %+v", got, want)
	}
}

func TestHarnessControlBypassesBackendAndHasSeparatePayloadSinks(t *testing.T) {
	j := Job{Case: testCase()}
	j.Case.CacheMode, j.Case.Workers, j.Case.ValueKind, j.Case.ValueBytes = "harness", 4, "bytes", 257
	cache, err := newCacheForJob(j, func(string, Case) (Cache, error) {
		t.Fatal("harness control must not construct the real backend")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	group := cache.(workerCaches)
	var wg sync.WaitGroup
	for w := 0; w < j.Case.Workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			child := group.ForWorker(w)
			for i := 0; i < 50; i++ {
				if err := child.Put("key", uint64(w)); err != nil {
					t.Error(err)
				}
				if child.Get("key") {
					t.Error("harness control must never claim a cache hit")
				}
				_ = cache.Stats() // Safe while all workers mutate their own sinks.
			}
		}(w)
	}
	wg.Wait()
	for w := 0; w < j.Case.Workers; w++ {
		s := group.ForWorker(w).(*harnessSink)
		if len(s.bytes) != j.Case.ValueBytes {
			t.Fatal("wrong payload size")
		}
		for i, b := range s.bytes {
			if b != byte(w+i) {
				t.Fatal("payload was not fully touched")
			}
		}
	}
	if cache.Stats() != (CacheStats{}) {
		t.Fatal("synthetic adapter reported real cache state")
	}
}

func TestTopologyConstructionErrorsAndSharedIdentity(t *testing.T) {
	j := Job{Case: testCase()}
	shared := newRecordingCache()
	for _, mode := range []string{"", "shared"} {
		j.Case.CacheMode = mode
		got, err := newCacheForJob(j, func(string, Case) (Cache, error) { return shared, nil })
		if err != nil || got != shared || cacheMultiplicity(j.Case) != 1 {
			t.Fatal("shared mode changed factory result", got, err)
		}
	}
	j.Case.CacheMode = "independent"
	wantErr := errors.New("construction failed")
	if _, err := newCacheForJob(j, func(string, Case) (Cache, error) { return nil, wantErr }); !errors.Is(err, wantErr) {
		t.Fatal("factory error was lost", err)
	}
	j.Case.DeleteMode = "prefix"
	if _, err := newCacheForJob(j, func(string, Case) (Cache, error) { return shared, nil }); err == nil {
		t.Fatal("independent mode masked missing prefix support")
	}
}
