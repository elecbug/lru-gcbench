package bench

import (
	"sync"
	"testing"
)

type recordingCache struct {
	mu           sync.Mutex
	items        map[string]bool
	hits, misses uint64
}

func newRecordingCache() *recordingCache { return &recordingCache{items: map[string]bool{}} }
func (c *recordingCache) Put(k string, id uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[k] = true
	return nil
}
func (c *recordingCache) Get(k string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.items[k]
	if v {
		c.hits++
	} else {
		c.misses++
	}
	return v
}
func (c *recordingCache) Delete(k string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.items[k]
	delete(c.items, k)
	return v
}
func (c *recordingCache) Compact() {}
func (c *recordingCache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{Entries: len(c.items), GetHits: c.hits, GetMisses: c.misses}
}
func TestUniqueKeys(t *testing.T) {
	for _, kind := range []string{"flat", "prefix"} {
		seen := map[string]bool{}
		for i := 0; i < 20000; i++ {
			k := MakeKey(uint64(i), kind)
			if seen[k] {
				t.Fatal("key collision", i)
			}
			seen[k] = true
			for _, b := range []byte(k) {
				if b >= 128 {
					t.Fatal("non-ASCII key")
				}
			}
		}
	}
}
func TestMixedCountsAndSamplingIndependentTrace(t *testing.T) {
	for _, workers := range []int{1, 3} {
		j := Job{Case: testCase(), Seed: 47}
		j.Case.Workers = workers
		a, err := prepareMixed(newRecordingCache(), j, 12345, 7).run()
		if err != nil {
			t.Fatal(err)
		}
		j.Case.LatencySampleEvery = 17
		b, err := prepareMixed(newRecordingCache(), j, 12345, 7).run()
		if err != nil {
			t.Fatal(err)
		}
		if a.operations != 12345 || a.operations != a.reads+a.writes || a.checksum != b.checksum || a.reads != b.reads || a.writes != b.writes {
			t.Fatal("sampling changed workload or operation count")
		}
		if b.get.summary().Samples+b.put.summary().Samples == 0 {
			t.Fatal("no request timing samples")
		}
	}
}
