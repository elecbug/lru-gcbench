package bench

import (
	"testing"
	"time"
)

func TestCalibrationRetainsAndTouchesIdenticalTraceBuffers(t *testing.T) {
	const capacity = 257
	off := newTracer(nil, 0, capacity, true)
	on := newTracer(nil, time.Millisecond, capacity, true)
	for _, tracer := range []*tracer{off, on} {
		if len(tracer.points) != capacity || cap(tracer.points) != capacity {
			t.Fatal("calibration sides have different trace buffer sizes")
		}
		for _, point := range tracer.points {
			if point.ElapsedNS != -1 {
				t.Fatal("buffer slot was not touched before baseline")
			}
		}
	}
	if off.stop != nil || off.done != nil || off.used != 0 {
		t.Fatal("buffer retention unexpectedly enabled periodic sampling")
	}
	off.start(nil, 0)
	off.finish()
	if unretained := newTracer(nil, 0, capacity); unretained.points != nil {
		t.Fatal("legacy disabled sampling should not allocate a buffer")
	}
}

type observedStatsCache struct {
	Cache
	observed chan struct{}
}

func (c observedStatsCache) Stats() CacheStats {
	select {
	case c.observed <- struct{}{}:
	default:
	}
	return CacheStats{Entries: 123, EvictionsPressure: 45, CompactionsPressureTier1: 2, CompactionsPressureTier2: 3}
}

func TestOptInCacheTraceRecordsRetentionAndPressure(t *testing.T) {
	c, err := newCollector()
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	observed := make(chan struct{}, 1)
	tracer := newTracer(c, time.Millisecond, 10)
	tracer.startWithCache(c, time.Millisecond, observedStatsCache{observed: observed})
	select {
	case <-observed:
	case <-timer.C:
		tracer.finish()
		t.Fatal("cache trace did not sample")
	}
	tracer.finish()
	if tracer.used == 0 {
		t.Fatal("cache sample was not retained")
	}
	for _, point := range tracer.points[:tracer.used] {
		if !point.CacheObserved || point.CacheEntries != 123 || point.PressureEvictions != 45 || point.PressureCompactions != 5 {
			t.Fatalf("bad cache observation: %+v", point)
		}
		if point.CacheElapsedNS < point.ElapsedNS {
			t.Fatal("cache observation timestamp precedes runtime sample")
		}
	}
}
