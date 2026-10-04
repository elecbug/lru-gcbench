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
		"performance": 30, "calibration": 150, "footprint": 120,
		"reclaim-study": 90, "scalability": 90, "pressure": 180, "study": 360,
		"topology-study": 180, "pressure-control": 90, "compact-window": 60,
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
				if v.LatencySampleEvery == 0 || v.Scenario == "footprint" || v.ReadPercent == 100 {
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
	if len(c.Cases) != 5 {
		t.Fatalf("got %d calibration arms, want 5", len(c.Cases))
	}
	base := c.Cases[0]
	wantRuntime := []bool{false, false, true, false, true}
	wantTiming := []bool{false, false, false, true, true}
	for i, arm := range c.Cases {
		if arm.RetainSampleBuffer != (i != 0) || (arm.SampleInterval != "0") != wantRuntime[i] || (arm.LatencySampleEvery != 0) != wantTiming[i] {
			t.Errorf("wrong instrumentation for calibration arm %d: %+v", i, arm)
		}
		arm.Name, arm.RetainSampleBuffer = base.Name, base.RetainSampleBuffer
		arm.SampleInterval, arm.LatencySampleEvery = base.SampleInterval, base.LatencySampleEvery
		if arm != base {
			t.Errorf("arm %d changes workload settings: %+v", i, arm)
		}
	}
}

func TestDiagnosticControlsPreserveInputs(t *testing.T) {
	topology := loadPerformanceExample(t, "topology-study")
	base := topology.Cases[0]
	for _, c := range topology.Cases {
		c.Name, c.CacheMode, c.Workers = base.Name, base.CacheMode, base.Workers
		if c != base {
			t.Errorf("topology control changes workload: %+v", c)
		}
	}
	harness := loadPerformanceExample(t, "harness-control")
	if len(harness.Backends) != 1 || len(harness.Jobs()) != 30 {
		t.Fatal("harness controls should not duplicate identical work under backend labels")
	}
	for _, c := range harness.Cases {
		if c.CacheMode != "harness" {
			t.Fatal("unlabelled harness control")
		}
	}
	pressure := loadPerformanceExample(t, "pressure-control")
	for _, c := range pressure.Cases {
		if c.KeySpace != 400000 || !c.SampleCacheStats {
			t.Fatalf("pressure controls must retain key space and observe retention: %+v", c)
		}
	}
	compact := loadPerformanceExample(t, "compact-window")
	a, b := compact.Cases[0], compact.Cases[1]
	if a.CompactMode != "disabled" || b.CompactMode != "enabled" || a.ReadPercent != 100 || a.RequestTraceEvery == 0 {
		t.Fatal("compact controls require read-only matched state and request traces")
	}
	a.Name, a.CompactMode = b.Name, b.CompactMode
	if a != b {
		t.Fatal("compact controls change settings other than the Compact call")
	}
	for _, name := range []string{"profiling", "profiling-arena", "profiling-arena-fill"} {
		for _, c := range loadPerformanceExample(t, name).Cases {
			if c.Profile == "" || c.ProfilePhase == "" {
				t.Errorf("%s has an unprofiled diagnostic arm", name)
			}
		}
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
