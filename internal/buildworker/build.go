package buildworker

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"example.com/lrugcbench/bench"
)

//go:embed worker_main.go.tmpl
var adapterTemplate string

func TreeHash(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "vendor" || d.Name() == "results" || d.Name() == "bin" {
				return filepath.SkipDir
			}
			return nil
		}
		// These are the compilation inputs used by this tool and this target library.
		name := d.Name()
		if !strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".tmpl") && name != "go.mod" && name != "go.sum" {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing ambiguous source symlink: %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00", filepath.ToSlash(rel))
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		h.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func moduleInfo(path string) (module, version string, err error) {
	b, err := os.ReadFile(filepath.Join(path, "go.mod"))
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			switch f[0] {
			case "module":
				module = strings.Trim(f[1], "\"")
			case "go":
				version = f[1]
			}
		}
	}
	if module == "" || version == "" {
		return "", "", fmt.Errorf("module/go directive not found in %s", path)
	}
	return module, version, nil
}
func git(repo string, args ...string) string {
	a := append([]string{"-C", repo}, args...)
	b, err := exec.Command("git", a...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func AdapterSource(p bench.Provenance) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return format.Source(fmt.Appendf(nil, adapterTemplate, strconv.Quote(string(b))))
}

// Build compiles against an EXISTING checkout. It does not fetch or modify it.
// The generated module is temporary and uses local replace directives.
func Build(ctx context.Context, repo, source, out string, log io.Writer) error {
	var err error
	repo, err = filepath.Abs(repo)
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return err
	}
	if _, err = os.Stat(out); err == nil {
		return fmt.Errorf("output exists; choose another -out path: %s", out)
	} else if !os.IsNotExist(err) {
		return err
	}
	mod, goVersion, err := moduleInfo(repo)
	if err != nil {
		return err
	}
	if mod != "github.com/google/go-lru" {
		return fmt.Errorf("target module is %q, expected github.com/google/go-lru", mod)
	}
	sourceMod, _, err := moduleInfo(source)
	if err != nil {
		return err
	}
	if sourceMod != "example.com/lrugcbench" {
		return fmt.Errorf("-source must point at the extracted lru-gcbench source tree")
	}
	targetHash, err := TreeHash(repo)
	if err != nil {
		return err
	}
	harnessHash, err := TreeHash(source)
	if err != nil {
		return err
	}
	p := bench.Provenance{Kind: "upstream-checkout", TargetModule: mod, TargetCommit: git(repo, "rev-parse", "HEAD"),
		TargetDirty: git(repo, "status", "--porcelain") != "", TargetTreeSHA256: targetHash, HarnessTreeSHA256: harnessHash, BuiltAt: time.Now().UTC().Format(time.RFC3339)}
	mainSource, err := AdapterSource(p)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "lrugcbench-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	module := fmt.Sprintf("module example.com/lrugcbenchworker\n\ngo %s\n\nrequire (\n github.com/google/go-lru v0.0.0\n example.com/lrugcbench v0.0.0\n)\n\nreplace github.com/google/go-lru => %s\nreplace example.com/lrugcbench => %s\n", goVersion, strconv.Quote(repo), strconv.Quote(source))
	if err = os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(module), 0600); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(tmp, "main.go"), mainSource, 0600); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(out), 0755); err != nil {
		return err
	}
	tempOut := out + ".building"
	if _, err = os.Stat(tempOut); err == nil {
		return fmt.Errorf("temporary output exists: %s", tempOut)
	}
	defer os.Remove(tempOut)
	cmd := exec.CommandContext(ctx, "go", "build", "-mod=mod", "-trimpath", "-buildvcs=false", "-o", tempOut, ".")
	cmd.Dir = tmp
	cmd.Env = bench.OverrideEnv(os.Environ(), map[string]string{"GOWORK": "off"})
	cmd.Stdout = log
	cmd.Stderr = log
	fmt.Fprintf(log, "Target: %s (commit %s, dirty=%t)\nGo directive: %s; source SHA256: %s\n", repo, p.TargetCommit, p.TargetDirty, goVersion, p.TargetTreeSHA256)
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("worker build failed; install the target's required Go toolchain: %w", err)
	}
	// Detect source changes during compilation rather than silently recording stale hashes.
	after, err := TreeHash(repo)
	if err != nil {
		return err
	}
	hafter, err := TreeHash(source)
	if err != nil {
		return err
	}
	if after != targetHash || hafter != harnessHash {
		return fmt.Errorf("source changed during build; rerun on a stable checkout")
	}
	if err = os.Rename(tempOut, out); err != nil {
		return err
	}
	fmt.Fprintf(log, "Built %s\n", out)
	return nil
}
