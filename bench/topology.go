package bench

import (
	"fmt"
	"runtime"
)

// workerCaches routes each request stream to a dedicated cache or harness sink.
// Fill, deletion and Compact still operate on the whole group.
type workerCaches interface {
	ForWorker(int) Cache
}

func cacheMultiplicity(c Case) int {
	if c.CacheMode == "independent" {
		return c.Workers
	}
	return 1
}

func newCacheForJob(j Job, factory Factory) (Cache, error) {
	switch j.Case.CacheMode {
	case "", "shared":
		return factory(j.Backend, j.Case)
	case "independent", "harness":
		group := &cacheGroup{caches: make([]Cache, j.Case.Workers)}
		for w := range group.caches {
			if j.Case.CacheMode == "harness" {
				group.caches[w] = &harnessSink{valueKind: j.Case.ValueKind, valueBytes: j.Case.ValueBytes}
				continue
			}
			cache, err := factory(j.Backend, j.Case)
			if err != nil {
				return nil, fmt.Errorf("independent cache %d: %w", w, err)
			}
			if j.Case.DeleteMode == "prefix" {
				if _, ok := cache.(PrefixCache); !ok {
					return nil, fmt.Errorf("independent cache %d does not support DeletePrefix", w)
				}
			}
			group.caches[w] = cache
		}
		return group, nil
	default:
		return nil, fmt.Errorf("unsupported cache_mode %q", j.Case.CacheMode)
	}
}

type cacheGroup struct {
	caches []Cache
}

func (g *cacheGroup) ForWorker(w int) Cache { return g.caches[w] }

// Put broadcasts only during fill. Mixed requests use ForWorker instead.
func (g *cacheGroup) Put(key string, id uint64) error {
	for _, cache := range g.caches {
		if err := cache.Put(key, id); err != nil {
			return err
		}
	}
	return nil
}

func (g *cacheGroup) Get(string) bool {
	panic("group Get requires selection with ForWorker")
}

func (g *cacheGroup) Delete(key string) bool {
	deleted := false
	for _, cache := range g.caches {
		// Do not short circuit: every cache must receive the deletion.
		if cache.Delete(key) {
			deleted = true
		}
	}
	return deleted
}

func (g *cacheGroup) DeletePrefix(prefix string) {
	for _, cache := range g.caches {
		cache.(PrefixCache).DeletePrefix(prefix)
	}
}

func (g *cacheGroup) Compact() {
	for _, cache := range g.caches {
		cache.Compact()
	}
}

func (g *cacheGroup) Stats() CacheStats {
	var total CacheStats
	for _, cache := range g.caches {
		s := cache.Stats()
		total.Entries += s.Entries
		total.CurrentSize += s.CurrentSize
		total.MaxSize += s.MaxSize
		total.GetHits += s.GetHits
		total.GetMisses += s.GetMisses
		total.EvictionsCapacity += s.EvictionsCapacity
		total.EvictionsPressure += s.EvictionsPressure
		total.EvictionsDeleted += s.EvictionsDeleted
		total.CompactionsExplicit += s.CompactionsExplicit
		total.CompactionsPressureTier1 += s.CompactionsPressureTier1
		total.CompactionsPressureTier2 += s.CompactionsPressureTier2
		total.CompactionsAutoSlack += s.CompactionsAutoSlack
		total.MemoryPressure = max(total.MemoryPressure, s.MemoryPressure)
		total.ArenaLiveNodes += s.ArenaLiveNodes
		total.ArenaFreeNodes += s.ArenaFreeNodes
		total.ArenaUnallocatedCap += s.ArenaUnallocatedCap
	}
	return total
}

// harnessSink has one owner during requests. It performs the adapter's payload
// work but stores no keys, never hits, and retains only the latest payload. Stats
// intentionally reports no cache activity, and does not read mutable sink data,
// so observation adds no locks or atomics to the request path.
type harnessSink struct {
	valueKind  string
	valueBytes int
	bytes      []byte
	scalar     [2]uint64
}

func (s *harnessSink) Put(key string, id uint64) error {
	if s.valueKind == "bytes" {
		v := make([]byte, s.valueBytes)
		for i := range v {
			v[i] = byte(id + uint64(i))
		}
		s.bytes = v
		runtime.KeepAlive(v)
	} else {
		s.scalar = [2]uint64{id, id ^ 0x9e3779b97f4a7c15}
	}
	runtime.KeepAlive(key)
	return nil
}

func (*harnessSink) Get(key string) bool { runtime.KeepAlive(key); return false }
func (*harnessSink) Delete(string) bool  { return false }
func (*harnessSink) DeletePrefix(string) {}
func (*harnessSink) Compact()            {}
func (*harnessSink) Stats() CacheStats   { return CacheStats{} }
