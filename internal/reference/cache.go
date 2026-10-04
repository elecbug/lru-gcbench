// Package reference is a TEST/VALIDATION cache, NOT google/go-lru. It is never
// substituted for a requested map/radix/arena backend. No performance conclusion
// about google/go-lru may be drawn from this implementation.
package reference

import (
	"container/list"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	"example.com/lrugcbench/bench"
)

//go:embed cache.go
var source []byte

func SourceHash() string { h := sha256.Sum256(source); return hex.EncodeToString(h[:]) }

type entry[V any] struct {
	key   string
	value V
}
type cache[V any] struct {
	mu        sync.Mutex
	items     map[string]*list.Element
	order     *list.List
	capacity  int
	makeValue func(uint64) V
	stats     bench.CacheStats
}

func newCache[V any](c bench.Case, makeValue func(uint64) V) bench.Cache {
	return &cache[V]{items: make(map[string]*list.Element), order: list.New(), capacity: c.Capacity, makeValue: makeValue}
}
func (c *cache[V]) Put(k string, id uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.makeValue(id)
	if e, ok := c.items[k]; ok {
		e.Value.(*entry[V]).value = v
		c.order.MoveToFront(e)
		return nil
	}
	e := c.order.PushFront(&entry[V]{key: k, value: v})
	c.items[k] = e
	if len(c.items) > c.capacity {
		last := c.order.Back()
		delete(c.items, last.Value.(*entry[V]).key)
		c.order.Remove(last)
		c.stats.EvictionsCapacity++
	}
	return nil
}
func (c *cache[V]) Get(k string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[k]
	if ok {
		c.stats.GetHits++
		c.order.MoveToFront(e)
	} else {
		c.stats.GetMisses++
	}
	return ok
}
func (c *cache[V]) Delete(k string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[k]
	if ok {
		delete(c.items, k)
		c.order.Remove(e)
		c.stats.EvictionsDeleted++
	}
	return ok
}
func (c *cache[V]) DeletePrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.items {
		if strings.HasPrefix(k, prefix) {
			delete(c.items, k)
			c.order.Remove(e)
			c.stats.EvictionsDeleted++
		}
	}
}
func (c *cache[V]) Compact() {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := make(map[string]*list.Element, len(c.items))
	for k, v := range c.items {
		m[k] = v
	}
	c.items = m
	c.stats.CompactionsExplicit++
}
func (c *cache[V]) Stats() bench.CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries = len(c.items)
	s.CurrentSize = uint64(len(c.items))
	s.MaxSize = uint64(c.capacity)
	return s
}
func Factory(name string, c bench.Case) (bench.Cache, error) {
	if name != "reference" {
		return nil, fmt.Errorf("reference worker rejects backend %q: it is not google/go-lru", name)
	}
	if c.PressureReclamation {
		return nil, fmt.Errorf("reference worker does not implement pressure reclamation")
	}
	switch c.ValueKind {
	case "scalar":
		return newCache(c, func(id uint64) [2]uint64 { return [2]uint64{id, id ^ 0x9e3779b97f4a7c15} }), nil
	case "bytes":
		return newCache(c, func(id uint64) []byte {
			v := make([]byte, c.ValueBytes)
			for i := range v {
				v[i] = byte(id + uint64(i))
			}
			return v
		}), nil
	default:
		return nil, fmt.Errorf("unsupported reference value kind")
	}
}
