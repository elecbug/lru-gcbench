package bench

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type invocationKey struct{}

// WithControllerInvocation records the actual CLI arguments (excluding argv[0]).
// Library callers may omit it; their process invocation is still recorded, but
// the replay script defaults to lrugcbench on PATH instead of their executable.
func WithControllerInvocation(ctx context.Context, args []string) context.Context {
	return context.WithValue(ctx, invocationKey{}, append([]string(nil), args...))
}

// RunCommand distinguishes the hosting process from the benchmark command. This
// matters for callers such as tests that invoke RunSuite as a library function.
type RunCommand struct {
	SchemaVersion     int       `json:"schema_version"`
	CreatedAt         time.Time `json:"created_at"`
	WorkingDirectory  string    `json:"working_directory"`
	ProcessExecutable string    `json:"process_executable"`
	ProcessSHA256     string    `json:"process_sha256"`
	ProcessArgs       []string  `json:"process_args"`
	CLIArgs           []string  `json:"cli_args,omitempty"`
	ReplayCommand     []string  `json:"replay_command"`
	ParentPairedRun   string    `json:"parent_paired_run,omitempty"`
	ReplayScope       string    `json:"replay_scope"`
}

type runArtifacts struct {
	command RunCommand
	file    *os.File
	log     io.Writer
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func writeRunArtifacts(ctx context.Context, out string, c Config, args []string, parent string, log io.Writer) (*runArtifacts, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	hash, err := FileHash(exe)
	if err != nil {
		return nil, err
	}
	cli, fromCLI := ctx.Value(invocationKey{}).([]string)
	controller := "lrugcbench"
	if fromCLI {
		controller = exe
	}
	command := RunCommand{
		SchemaVersion: SchemaVersion, CreatedAt: time.Now().UTC(), WorkingDirectory: cwd,
		ProcessExecutable: exe, ProcessSHA256: hash, ProcessArgs: append([]string(nil), os.Args...),
		CLIArgs: cli, ReplayCommand: append([]string{controller}, args...), ParentPairedRun: parent,
		ReplayScope: "standalone suite",
	}
	if args[0] == "run-paired" {
		command.ReplayScope = "paired run with original alternation order"
	} else if parent != "" {
		command.ReplayScope = "individual side; use the parent reproduce.sh to preserve paired execution order"
	}
	if err = WriteJSON(filepath.Join(out, "config.json"), c); err != nil {
		return nil, err
	}
	if err = WriteJSON(filepath.Join(out, "command.json"), command); err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(out, "reproduce.sh"), []byte(reproductionScript(command)), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(out, "run.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = io.Discard
	}
	a := &runArtifacts{command: command, file: f, log: io.MultiWriter(log, f)}
	fmt.Fprintf(a.log, "Run started: %s\nReplay script: %s\n", command.CreatedAt.Format(time.RFC3339), filepath.Join(out, "reproduce.sh"))
	return a, nil
}

func reproductionScript(command RunCommand) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Replay the captured configuration into a NEW output directory.\n")
	b.WriteString("# Usage: ./reproduce.sh /path/to/new-results\n")
	b.WriteString("# Overrides: LRUGCBENCH_CONTROLLER, LRUGCBENCH_WORKER (single run),\n# LRUGCBENCH_BASE_WORKER and LRUGCBENCH_CANDIDATE_WORKER (paired run).\n")
	if command.ParentPairedRun != "" {
		b.WriteString("# This is one side of a paired run. Use the parent's script to preserve order.\n")
	}
	b.WriteString("set -eu\nif [ \"$#\" -ne 1 ] || [ -z \"$1\" ]; then\n  echo 'Usage: reproduce.sh NEW_OUTPUT_DIRECTORY' >&2\n  exit 2\nfi\n")
	b.WriteString("if [ -e \"$1\" ] || [ -L \"$1\" ]; then\n  echo 'Output already exists; choose a new directory.' >&2\n  exit 2\nfi\n")
	fmt.Fprintf(&b, "controller=${LRUGCBENCH_CONTROLLER:-%s}\n", shellQuote(command.ReplayCommand[0]))
	args := command.ReplayCommand[1:]
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-worker":
			fmt.Fprintf(&b, "worker=${LRUGCBENCH_WORKER:-%s}\n", shellQuote(args[i+1]))
		case "-base-worker":
			fmt.Fprintf(&b, "base_worker=${LRUGCBENCH_BASE_WORKER:-%s}\n", shellQuote(args[i+1]))
		case "-candidate-worker":
			fmt.Fprintf(&b, "candidate_worker=${LRUGCBENCH_CANDIDATE_WORKER:-%s}\n", shellQuote(args[i+1]))
		}
		if arg != "-allow-env-diff" && arg != "-allow-harness-diff" {
			i++ // Values may themselves look like flags.
		}
	}
	b.WriteString("exec \"$controller\"")
	for i := 0; i < len(args); i++ {
		arg := args[i]
		b.WriteByte(' ')
		b.WriteString(shellQuote(arg))
		switch arg {
		case "-out":
			b.WriteString(" \"$1\"")
			i++
		case "-worker":
			b.WriteString(" \"$worker\"")
			i++
		case "-base-worker":
			b.WriteString(" \"$base_worker\"")
			i++
		case "-candidate-worker":
			b.WriteString(" \"$candidate_worker\"")
			i++
		case "-config", "-label", "-base-label", "-candidate-label":
			i++
			b.WriteByte(' ')
			b.WriteString(shellQuote(args[i]))
		}
	}
	b.WriteByte('\n')
	return b.String()
}

func (a *runArtifacts) finish(err error) error {
	if err != nil {
		fmt.Fprintf(a.log, "Run failed: %v\n", err)
	} else {
		fmt.Fprintln(a.log, "Run completed successfully.")
	}
	return a.file.Close()
}

func suiteReplayArgs(s *suiteRunner) []string {
	return []string{"run", "-worker", s.worker, "-config", filepath.Join(s.out, "config.json"), "-out", s.out, "-label", s.manifest.Label}
}
