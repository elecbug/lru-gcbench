package bench

import (
	"path/filepath"
	"reflect"
	"testing"
)

func loadPerformanceExample(t *testing.T, name string) Config {
	t.Helper()
	c, err := LoadConfig(filepath.Join("..", "examples", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// These examples define the study protocol, so check that edits do not silently
// remove repetitions or a comparison arm from the published experiment matrix.
func TestPerformanceExampleMatrices(t *testing.T) {
	for name, wantJobs := range map[string]int{
		"performance": 30, "calibration": 60, "footprint": 120,
		"reclaim-study": 90, "scalability": 90, "pressure": 180, "study": 360,
	} {
		t.Run(name, func(t *testing.T) {
			c := loadPerformanceExample(t, name)
			if c.Repetitions < 10 {
				t.Fatalf("only %d repetitions; performance studies require at least 10", c.Repetitions)
			}
			if !reflect.DeepEqual(c.Backends, []string{"map", "radix", "arena"}) {
				t.Fatalf("incomplete backend comparison: %v", c.Backends)
			}
			if got := len(c.Jobs()); got != wantJobs {
				t.Fatalf("got %d jobs, want %d", got, wantJobs)
			}
			for _, v := range c.Cases {
				if v.LatencySampleEvery == 0 || v.Scenario == "footprint" {
					continue
				}
				// This is the expected count under the requested operation mix,
				// not a guarantee of actual samples or stable p99 estimates.
				expectedPutSamples := float64(v.Operations) * float64(100-v.ReadPercent) / 100 / float64(v.LatencySampleEvery)
				if expectedPutSamples < 1000 {
					t.Errorf("%s expects only %.0f Put latency samples", v.Name, expectedPutSamples)
				}
			}
		})
	}
}

func TestCalibrationChangesOnlyInstrumentation(t *testing.T) {
	c := loadPerformanceExample(t, "calibration")
	if len(c.Cases) != 2 {
		t.Fatalf("got %d calibration arms, want 2", len(c.Cases))
	}
	off, on := c.Cases[0], c.Cases[1]
	if off.SampleInterval != "0" || off.LatencySampleEvery != 0 {
		t.Fatal("instrumentation-off still enables periodic sampling or request timing")
	}
	if on.SampleInterval == "0" || on.LatencySampleEvery == 0 {
		t.Fatal("instrumentation-on does not enable both periodic sampling and request timing")
	}
	off.Name = on.Name
	off.SampleInterval = on.SampleInterval
	off.LatencySampleEvery = on.LatencySampleEvery
	if off != on {
		t.Fatalf("calibration arms differ in workload or allocation settings:\noff: %+v\non:  %+v", off, on)
	}
}

func TestPerformanceBaselineHasNoMemoryPressureConfound(t *testing.T) {
	for _, name := range []string{"performance", "calibration", "footprint", "reclaim-study", "scalability"} {
		t.Run(name, func(t *testing.T) {
			c := loadPerformanceExample(t, name)
			for _, p := range c.Runtimes {
				if p.GOMEMLIMIT != "off" || p.GOGC != 100 {
					t.Errorf("baseline changes GC policy: %+v", p)
				}
			}
			for _, v := range c.Cases {
				if v.PressureReclamation {
					t.Errorf("%s enables pressure reclamation", v.Name)
				}
				if v.Scenario == "footprint" && v.SampleInterval != "0" {
					t.Errorf("%s enables periodic sampling in a forced-GC footprint study", v.Name)
				}
			}
		})
	}
}

func TestScalabilityKeepsRuntimeParallelismFixed(t *testing.T) {
	c := loadPerformanceExample(t, "scalability")
	if len(c.Runtimes) != 1 || c.Runtimes[0].GOMAXPROCS != 4 {
		t.Fatalf("scalability must hold GOMAXPROCS at 4: %+v", c.Runtimes)
	}
	var workers []int
	baseline := c.Cases[0]
	for _, v := range c.Cases {
		workers = append(workers, v.Workers)
		v.Name, v.Workers = baseline.Name, baseline.Workers
		if v != baseline {
			t.Errorf("scalability changes settings other than workers: %+v", v)
		}
	}
	if !reflect.DeepEqual(workers, []int{1, 4, 8}) {
		t.Fatalf("got worker levels %v, want [1 4 8]", workers)
	}
}
