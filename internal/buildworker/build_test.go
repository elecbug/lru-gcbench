package buildworker

import (
	"context"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"example.com/lrugcbench/bench"
)

func TestGeneratedAdapterSyntax(t *testing.T) {
	src, err := AdapterSource(bench.Provenance{Kind: "upstream-checkout", TargetModule: "github.com/google/go-lru", TargetCommit: "quotes\"and\\slashes"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = parser.ParseFile(token.NewFileSet(), "main.go", src, parser.AllErrors); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "WithPressureFunc") {
		t.Fatal("disabled pressure must not install a custom callback")
	}
}
func TestTreeHashTracksSourceNotBinaries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.go")
	os.WriteFile(p, []byte("package a"), 0600)
	a, err := TreeHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "program.exe"), []byte("binary"), 0600)
	b, _ := TreeHash(dir)
	if a != b {
		t.Fatal("binary changed source hash")
	}
	os.WriteFile(p, []byte("package b"), 0600)
	b, _ = TreeHash(dir)
	if a == b {
		t.Fatal("source changes not tracked")
	}
}
func TestBuildAdapterAgainstAPIFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("compile-only API fixture")
	}
	// This fixture ONLY type-checks the generated adapter. Every constructor panics.
	// It is NOT an implementation of google/go-lru and produces NO benchmark data.
	fixture := `package lru
 type Backend int
 const(BackendMap Backend=iota;BackendRadix;BackendArenaRadix)
 type Option func()
 func WithBackend(Backend)Option{return nil}
 func WithCompactionThreshold(float64)Option{return nil}
 func WithEvictionThreshold(float64)Option{return nil}
 type Stats struct{Len int;CurrentSize,MaxSize,GetHits,GetMisses,EvictionsCapacity,EvictionsPressure,EvictionsDeleted,CompactionsExplicit,CompactionsPressureTier1,CompactionsPressureTier2,CompactionsAutoSlack uint64;MemoryPressure float64;ArenaLiveNodes,ArenaFreeNodes,ArenaUnallocatedCap int}
 type Cache[V any]interface{Put(string,V)([]V,error);Get(string)(V,bool);Delete(string)(V,bool);DeletePrefix(string);Stats()Stats}
 type PressureAwareCache[V any]interface{Cache[V];Compact()}
 func New[V any](uint64,...Option)Cache[V]{panic("COMPILE-ONLY API FIXTURE; NOT A REAL CACHE")}
 `
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module github.com/google/go-lru\n\ngo 1.23.0\n"), 0600)
	os.WriteFile(filepath.Join(repo, "api_fixture.go"), []byte(fixture), 0600)
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	name := "fixture-worker"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	if err = Build(context.Background(), repo, source, out, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(out, "--describe").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "arena") {
		t.Fatal("adapter capabilities not encoded")
	}
	if err = Build(context.Background(), repo, source, out, io.Discard); err == nil {
		t.Fatal("existing worker overwritten")
	}
}
