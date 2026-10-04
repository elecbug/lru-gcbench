package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestReplayScriptPreservesArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell replay")
	}
	for _, paired := range []bool{false, true} {
		t.Run(map[bool]string{false: "suite", true: "paired"}[paired], func(t *testing.T) {
			dir := t.TempDir()
			// All substitutions must be inert data, including newlines and values
			// that look like flags. A recorder acts as the controller.
			controller := filepath.Join(dir, "controller ' $HOME `id` $(id)")
			if err := os.WriteFile(controller, []byte("#!/bin/sh\nprintf '%s\\000' \"$@\" > \"$LRUGCBENCH_CAPTURE\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			worker := filepath.Join(dir, "worker ' ${HOME} `id` $(id)\nspace")
			config := filepath.Join(dir, "config ' $(id).json")
			originalOut := filepath.Join(dir, "original")
			args := []string{"run", "-worker", worker, "-config", config, "-out", originalOut, "-label", "-out"}
			if paired {
				args = []string{"run-paired", "-base-worker", worker, "-candidate-worker", worker + " candidate", "-config", config, "-out", originalOut, "-base-label", "-worker", "-candidate-label", "quote ' $(id)\nlabel", "-allow-env-diff", "-allow-harness-diff"}
			}
			script := filepath.Join(dir, "reproduce.sh")
			command := RunCommand{ReplayCommand: append([]string{controller}, args...)}
			if err := os.WriteFile(script, []byte(reproductionScript(command)), 0755); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("sh", "-n", script).CombinedOutput(); err != nil {
				t.Fatalf("shell syntax: %v\n%s", err, output)
			}
			capture := filepath.Join(dir, "capture")
			out := filepath.Join(dir, "replayed ' $(id)\noutput")
			cmd := exec.Command(script, out)
			cmd.Env = OverrideEnv(os.Environ(), map[string]string{"LRUGCBENCH_CAPTURE": capture, "LRUGCBENCH_CONTROLLER": "", "LRUGCBENCH_WORKER": "", "LRUGCBENCH_BASE_WORKER": "", "LRUGCBENCH_CANDIDATE_WORKER": ""})
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("replay: %v\n%s", err, output)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			want := append([]string(nil), args...)
			for i := range want {
				if want[i] == originalOut {
					want[i] = out
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("arguments changed:\ngot  %#v\nwant %#v", got, want)
			}
			// Environment overrides remain one argument even with shell syntax.
			override := "replacement ' $(id)"
			cmd = exec.Command(script, out)
			cmd.Env = OverrideEnv(os.Environ(), map[string]string{"LRUGCBENCH_CAPTURE": capture, "LRUGCBENCH_CONTROLLER": controller, "LRUGCBENCH_WORKER": override, "LRUGCBENCH_BASE_WORKER": override, "LRUGCBENCH_CANDIDATE_WORKER": override})
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("override replay: %v\n%s", err, output)
			}
			data, _ = os.ReadFile(capture)
			got = strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
			for i, arg := range args {
				if arg == "-worker" || arg == "-base-worker" || arg == "-candidate-worker" {
					// Only inspect real flags, not their values.
					if i == 1 || paired && i == 3 {
						want[i+1] = override
					}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("override arguments changed: %#v != %#v", got, want)
			}
			if err := os.Mkdir(out, 0755); err != nil {
				t.Fatal(err)
			}
			if err := exec.Command(script, out).Run(); err == nil {
				t.Fatal("existing output accepted")
			}
			if err := exec.Command(script).Run(); err == nil {
				t.Fatal("missing output accepted")
			}
		})
	}
}

func TestRunArtifactMetadataAndLogging(t *testing.T) {
	dir := t.TempDir()
	c := testConfig()
	args := []string{"run", "-worker", "/worker", "-config", filepath.Join(dir, "config.json"), "-out", dir, "-label", "test"}
	cli := []string{"run", "-config", "input.json", "-worker", "../worker", "-out", dir}
	ctx := WithControllerInvocation(context.Background(), cli)
	var progress bytes.Buffer
	a, err := writeRunArtifacts(ctx, dir, c, args, "/paired", &progress)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.log.Write([]byte("worker progress\n")); err != nil {
		t.Fatal(err)
	}
	if err = a.finish(nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "run.log"))
	if err != nil || !bytes.Equal(data, progress.Bytes()) || !strings.Contains(string(data), "worker progress") {
		t.Fatalf("log not mirrored: %v %s", err, data)
	}
	loaded, err := LoadConfig(filepath.Join(dir, "config.json"))
	if err != nil || !reflect.DeepEqual(loaded, c) {
		t.Fatalf("resolved config differs: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(dir, "command.json"))
	if err != nil {
		t.Fatal(err)
	}
	var command RunCommand
	if err = json.Unmarshal(data, &command); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(command.ProcessArgs, os.Args) || !reflect.DeepEqual(command.CLIArgs, cli) || command.ProcessSHA256 == "" || command.WorkingDirectory == "" || command.ParentPairedRun != "/paired" || command.ReplayCommand[0] != command.ProcessExecutable {
		t.Fatalf("missing command metadata: %+v", command)
	}
	st, err := os.Stat(filepath.Join(dir, "reproduce.sh"))
	if err != nil || st.Mode().Perm()&0111 == 0 {
		t.Fatalf("script is not executable: %v", err)
	}
}
