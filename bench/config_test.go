package bench

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testCase() Case {
	return Case{Name: "test", Scenario: "steady", Capacity: 100, KeySpace: 200, KeyKind: "flat", ValueKind: "scalar", Workers: 1, WarmupOps: 100, Operations: 1000, ReadPercent: 75, DeleteFraction: .75, SampleInterval: "0", MaxSamples: 64}
}
func testConfig() Config {
	return Config{SchemaVersion: 1, Repetitions: 3, Seed: 42, Timeout: "5s", Backends: []string{"a", "b"}, Runtimes: []RuntimeConfig{{Name: "base", GOMAXPROCS: 1, GOGC: 100, GOMEMLIMIT: "off"}}, Cases: []Case{testCase()}}
}
func TestMemoryLimit(t *testing.T) {
	cases := map[string]int64{"off": math.MaxInt64, "1B": 1, "2KiB": 2048, "128MiB": 128 << 20, "3GiB": 3 << 30, "1TiB": 1 << 40}
	for s, want := range cases {
		got, err := MemoryLimit(s)
		if err != nil || got != want {
			t.Fatalf("%s: %d %v", s, got, err)
		}
	}
	for _, s := range []string{"", "1GB", "-1MiB", "0B", "999999999999999GiB", "1.5MiB", "123"} {
		if _, err := MemoryLimit(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
func TestStrictDecode(t *testing.T) {
	for _, s := range []string{`{"unknown":1}`, `{} {}`, `{} trailing`} {
		var c Config
		if err := DecodeStrict(strings.NewReader(s), &c); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}
func TestLoadDefaultsAndExplicitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"backends":["reference"],"runtimes":[{"name":"default","gogc":0}],"cases":[{"name":"small","capacity":32,"warmup_ops":0,"read_percent":0,"sample_interval":"0"}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Cases[0].KeySpace != 64 || c.Cases[0].WarmupOps != 0 || c.Cases[0].ReadPercent != 0 || c.Runtimes[0].GOGC != 0 {
		t.Fatalf("bad defaults: %+v", c)
	}
}
func TestValidation(t *testing.T) {
	mutations := []func(*Config){func(c *Config) { c.Repetitions = 0 }, func(c *Config) { c.Cases[0].Capacity = 0 }, func(c *Config) { c.Cases[0].KeySpace = 1 }, func(c *Config) { c.Cases[0].SampleInterval = "1us" }, func(c *Config) { c.Cases[0].ValueKind = "bytes" }, func(c *Config) { c.Cases[0].Scenario = "footprint"; c.Cases[0].PressureReclamation = true }, func(c *Config) { c.Backends = []string{"a", "a"} }, func(c *Config) { c.Timeout = "0" }}
	for i, f := range mutations {
		c := testConfig()
		f(&c)
		if c.Validate() == nil {
			t.Errorf("mutation %d accepted", i)
		}
	}
}
func TestJobDeterminismAndPairedSeeds(t *testing.T) {
	c := testConfig()
	a, b := c.Jobs(), c.Jobs()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("schedule nondeterministic")
	}
	ids := map[string]bool{}
	for _, j := range a {
		if ids[j.ID] {
			t.Fatal("duplicate job ID")
		}
		ids[j.ID] = true
		if j.Seed != c.Seed+int64(j.Repeat) {
			t.Fatal("backend-dependent workload seed")
		}
	}
	if len(a) != 6 {
		t.Fatal(len(a))
	}
	j := a[0]
	before := GroupKey(j, "measured")
	j.Seed++
	j.Repeat++
	if before != GroupKey(j, "measured") {
		t.Fatal("seed leaked into group")
	}
	j.Case.ReadPercent--
	if before == GroupKey(j, "measured") {
		t.Fatal("different workloads grouped")
	}
}
func TestOverrideEnv(t *testing.T) {
	got := OverrideEnv([]string{"GOGC=10", "PATH=/bin", "gomaxprocs=4"}, map[string]string{"GOGC": "off", "GOMAXPROCS": "2"})
	joined := strings.Join(got, ";")
	if strings.Contains(joined, "GOGC=10") || strings.Contains(joined, "gomaxprocs=4") || !strings.Contains(joined, "GOGC=off") {
		t.Fatal(got)
	}
}
func TestJSONRoundTrip(t *testing.T) {
	j := testConfig().Jobs()[0]
	b, _ := json.Marshal(j)
	var k Job
	if err := DecodeStrict(strings.NewReader(string(b)), &k); err != nil || !reflect.DeepEqual(j, k) {
		t.Fatalf("roundtrip: %v", err)
	}
}

func TestAllExampleConfigs(t *testing.T) {
	paths, err := filepath.Glob("../examples/*.json")
	if err != nil || len(paths) < 6 {
		t.Fatalf("examples: %v %v", paths, err)
	}
	for _, p := range paths {
		c, err := LoadConfig(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if len(c.Jobs()) == 0 {
			t.Fatal("empty schedule", p)
		}
	}
}

func TestDeletionAndConcurrentConfigValidation(t *testing.T) {
	valid := testCase()
	valid.Scenario = "reclaim"
	valid.KeyKind = "prefix"
	valid.DeleteMode = "prefix"
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Case){
		func(c *Case) { c.KeyKind = "flat" },
		func(c *Case) { c.Scenario = "steady" },
		func(c *Case) { c.DeleteFraction = .7 },
		func(c *Case) { c.DeleteMode = "unknown" },
	} {
		c := valid
		mutate(&c)
		if c.Validate() == nil {
			t.Fatalf("invalid deletion config accepted: %+v", c)
		}
	}
	c := testCase()
	c.Scenario = "concurrent-compact"
	if c.Validate() == nil {
		t.Fatal("concurrent compaction accepted without request sampling")
	}
	c.LatencySampleEvery = 10
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Operations = 1
	if c.Validate() == nil {
		t.Fatal("too few operations accepted")
	}
}
