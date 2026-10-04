package bench

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// The first request after the midpoint waits for Compact to enter; Compact
// waits for that request. A sequential implementation cannot finish this test.
type overlapCache struct {
	*recordingCache
	requests      atomic.Int64
	compactions   atomic.Int64
	midpoint      int64
	entered       chan struct{}
	requestDuring chan struct{}
}

func (c *overlapCache) Get(k string) bool {
	if c.requests.Add(1) == c.midpoint+1 {
		<-c.entered
		close(c.requestDuring)
	}
	return c.recordingCache.Get(k)
}
func (c *overlapCache) Compact() {
	c.compactions.Add(1)
	close(c.entered)
	<-c.requestDuring
}
func TestConcurrentCompactionOverlapsWorkload(t *testing.T) {
	const n = 2000
	c := &overlapCache{recordingCache: newRecordingCache(), midpoint: n / 2, entered: make(chan struct{}), requestDuring: make(chan struct{})}
	j := Job{Case: testCase(), Seed: 42}
	j.Case.ReadPercent = 100
	task := prepareMixedTask(c, j, n, 7, true)
	type outcome struct {
		counts opCounts
		err    error
	}
	result := make(chan outcome, 1)
	go func() { v, e := task.run(); result <- outcome{v, e} }()
	select {
	case r := <-result:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.counts.operations != n || c.compactions.Load() != 1 {
			t.Fatalf("counts: %d compactions: %d", r.counts.operations, c.compactions.Load())
		}
		if task.compactionStart.IsZero() || task.compactionEnd.Before(task.compactionStart) {
			t.Fatal("invalid compaction window")
		}
		plain, err := prepareMixed(newRecordingCache(), j, n, 7).run()
		if err != nil || r.counts.checksum != plain.checksum || r.counts.reads != plain.reads {
			t.Fatal("compaction changed input stream")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Compact did not overlap requests or worker deadlocked")
	}
}

type failingPutCache struct {
	*recordingCache
	compactions atomic.Int64
}

func (c *failingPutCache) Put(string, uint64) error { return errors.New("injected failure") }
func (c *failingPutCache) Compact()                 { c.compactions.Add(1) }
func TestConcurrentCompactionReleasesOnEarlyFailure(t *testing.T) {
	c := &failingPutCache{recordingCache: newRecordingCache()}
	j := Job{Case: testCase(), Seed: 42}
	j.Case.ReadPercent = 0
	task := prepareMixedTask(c, j, 100, 7, true)
	finished := make(chan error, 1)
	go func() { _, err := task.run(); finished <- err }()
	select {
	case err := <-finished:
		if err == nil || c.compactions.Load() != 1 {
			t.Fatalf("err=%v compactions=%d", err, c.compactions.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("early failure left compactor waiting")
	}
}

func TestPrefixDeletionCountUnevenGroups(t *testing.T) {
	for capacity := 1; capacity < 100; capacity++ {
		for groups := 1; groups < 16; groups++ {
			c := testCase()
			c.Capacity = capacity
			c.DeleteMode = "prefix"
			c.DeleteFraction = float64(groups) / 16
			want := 0
			for id := 0; id < capacity; id++ {
				if id%16 < groups {
					want++
				}
			}
			if got := deletedEntryCount(c); got != want {
				t.Fatalf("capacity=%d groups=%d got=%d want=%d", capacity, groups, got, want)
			}
		}
	}
}
