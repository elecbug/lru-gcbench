package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"path/filepath"
	"time"
)

// PairedStep records both the planned order and the completed status. Base and
// candidate jobs have identical IDs and input settings, and run consecutively.
type PairedStep struct {
	Pair   int    `json:"pair"`
	Side   string `json:"side"`
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

type PairedManifest struct {
	SchemaVersion       int          `json:"schema_version"`
	Seed                int64        `json:"seed"`
	CreatedAt           time.Time    `json:"created_at"`
	FinishedAt          *time.Time   `json:"finished_at,omitempty"`
	EnvironmentOverride bool         `json:"environment_override"`
	HarnessOverride     bool         `json:"harness_override"`
	Steps               []PairedStep `json:"steps"`
	Error               string       `json:"error,omitempty"`
}

func pairedSteps(jobs []Job, seed int64) []PairedStep {
	first := rand.New(rand.NewSource(seed)).Intn(2)
	sides := []string{"base", "candidate"}
	steps := make([]PairedStep, 0, 2*len(jobs))
	for i, job := range jobs {
		for offset := 0; offset < 2; offset++ {
			steps = append(steps, PairedStep{Pair: i, Side: sides[(first+i+offset)%2], JobID: job.ID, Status: "pending"})
		}
	}
	return steps
}

// RunPaired alternates which side runs first, with the starting side chosen by
// the config seed. Every step is a fresh process, and no steps run concurrently.
// A comparison is written only when both suites finish without failed jobs.
func RunPaired(ctx context.Context, baseWorker, candidateWorker string, c Config, out, baseLabel, candidateLabel string, options CompareOptions, log io.Writer) (manifest PairedManifest, runErr error) {
	m := PairedManifest{SchemaVersion: SchemaVersion, Seed: c.Seed, CreatedAt: time.Now().UTC(), EnvironmentOverride: options.AllowEnvironmentDiff, HarnessOverride: options.AllowHarnessDiff}
	base, err := prepareSuite(baseWorker, c, filepath.Join(out, "base"), baseLabel)
	if err != nil {
		return m, fmt.Errorf("baseline: %w", err)
	}
	candidate, err := prepareSuite(candidateWorker, c, filepath.Join(out, "candidate"), candidateLabel)
	if err != nil {
		return m, fmt.Errorf("candidate: %w", err)
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return m, err
	}
	if err = reserveOutputDir(out); err != nil {
		return m, err
	}
	m.Steps = pairedSteps(c.Jobs(), c.Seed)
	manifestPath := filepath.Join(out, "paired.json")
	if err = WriteJSON(manifestPath, m); err != nil {
		return m, err
	}
	args := []string{"run-paired", "-base-worker", base.worker, "-candidate-worker", candidate.worker, "-config", filepath.Join(out, "config.json"), "-out", out, "-base-label", baseLabel, "-candidate-label", candidateLabel}
	if options.AllowEnvironmentDiff {
		args = append(args, "-allow-env-diff")
	}
	if options.AllowHarnessDiff {
		args = append(args, "-allow-harness-diff")
	}
	artifacts, err := writeRunArtifacts(ctx, out, c, args, "", log)
	if err != nil {
		return finishPaired(m, manifestPath, err)
	}
	defer func() { runErr = errors.Join(runErr, artifacts.finish(runErr)) }()
	log = artifacts.log
	if err = base.initialize(); err != nil {
		return finishPaired(m, manifestPath, err)
	}
	if err = candidate.initialize(); err != nil {
		return finishPaired(m, manifestPath, errors.Join(err, base.finish(ctx)))
	}
	baseArtifacts, err := writeRunArtifacts(ctx, base.out, c, suiteReplayArgs(base), out, log)
	if err != nil {
		return finishPaired(m, manifestPath, err)
	}
	defer func() { runErr = errors.Join(runErr, baseArtifacts.finish(runErr)) }()
	candidateArtifacts, err := writeRunArtifacts(ctx, candidate.out, c, suiteReplayArgs(candidate), out, log)
	if err != nil {
		return finishPaired(m, manifestPath, err)
	}
	defer func() { runErr = errors.Join(runErr, candidateArtifacts.finish(runErr)) }()
	if err = base.inspectWorker(ctx); err == nil {
		err = candidate.inspectWorker(ctx)
	}
	if err == nil {
		err = compareProvenance(base.manifest.Capabilities.Provenance, candidate.manifest.Capabilities.Provenance, options)
	}
	if err != nil {
		// Startup errors still leave the captured command, config and logs on
		// both sides, even when a worker cannot describe itself.
		now := time.Now().UTC()
		for _, suite := range []*suiteRunner{base, candidate} {
			suite.manifest.Error = err.Error()
			suite.manifest.FinishedAt = &now
			err = errors.Join(err, suite.save())
		}
		return finishPaired(m, manifestPath, err)
	}
	for i := range m.Steps {
		if ctx.Err() != nil {
			break
		}
		step := &m.Steps[i]
		suite := base
		stepLog := baseArtifacts.log
		if step.Side == "candidate" {
			suite = candidate
			stepLog = candidateArtifacts.log
		}
		err = suite.runJob(ctx, step.Pair, stepLog)
		step.Status = suite.manifest.Jobs[step.Pair].Status
		if err != nil {
			break
		}
		if err = WriteJSON(manifestPath, m); err != nil {
			break
		}
	}
	// Finalize both manifests, even if one has failed or the caller cancelled.
	err = errors.Join(err, base.finish(ctx), candidate.finish(ctx))
	for i := range m.Steps {
		step := &m.Steps[i]
		suite := base
		if step.Side == "candidate" {
			suite = candidate
		}
		step.Status = suite.manifest.Jobs[step.Pair].Status
	}
	if err == nil {
		err = CompareWithOptions(base.out, candidate.out, filepath.Join(out, "comparison"), options)
	}
	return finishPaired(m, manifestPath, err)
}

func finishPaired(m PairedManifest, manifestPath string, err error) (PairedManifest, error) {
	if err != nil {
		m.Error = err.Error()
	}
	now := time.Now().UTC()
	m.FinishedAt = &now
	return m, errors.Join(err, WriteJSON(manifestPath, m))
}
