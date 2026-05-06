package handler

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// runtimeKind names where a quality check executes. "stack" means inside a
// one-shot Multipass VM via `stack run --json`. "host" means in the operator's
// own process tree via `exec.Command` with cwd=workspace; reserved for repos
// that cannot be stack-virtualized (e.g. Go monorepos with no docker-compose).
type runtimeKind string

const (
	runtimeStack runtimeKind = "stack"
	runtimeHost  runtimeKind = "host"
)

// quality.yaml lives under .rick/ at the workspace root. The path is fixed,
// not configurable — repo teams own a single canonical contract.
const qualityManifestRelPath = ".rick/quality.yaml"

// QualityManifest is the on-disk schema for `.rick/quality.yaml`. The repo
// declares which runtime to use and the exact commands to drive lint/test;
// rick does not infer them. Fields use snake_case to match standard yaml/CI
// idioms.
type QualityManifest struct {
	// Runtime selects "stack" (default) or "host". A host-runtime manifest
	// is rejected at load time when RICK_ALLOW_HOST_RUNTIME=1 is not set —
	// see validateManifest.
	Runtime runtimeKind `yaml:"runtime"`

	// Checks lists the commands rick will invoke, in order. Each check's
	// failure stops further checks for that runtime invocation only when
	// the runtime is set up that way; today we run them sequentially and
	// collect all failures before emitting the verdict.
	Checks []ManifestCheck `yaml:"checks"`
}

// ManifestCheck is one quality check. The Command slice goes verbatim to
// `stack run -- <command>` (stack runtime) or `exec.Command(command[0],
// command[1:]...)` (host runtime). Wrappers like `bash -c "..."` are the
// caller's responsibility — rick does no shell parsing.
type ManifestCheck struct {
	// Name is the logical identifier (e.g. "lint", "test"). Used in
	// summaries, debug filenames, and persona feedback.
	Name string `yaml:"name"`

	// Label is the human-readable command form shown in operator-facing
	// failure descriptions. When omitted, falls back to strings.Join(Command, " ").
	Label string `yaml:"label,omitempty"`

	// Command is the argv passed to the runtime. First element is the
	// executable; remaining are args. No shell expansion is performed by rick.
	Command []string `yaml:"command"`
}

// loadQualityManifest reads .rick/quality.yaml from the workspace root.
//
// Three return shapes:
//   - (manifest, nil) — file exists, parses cleanly, validates.
//   - (nil, nil) — file does not exist; caller falls back to legacy probing
//     (run.sh, Makefile). Backward-compatible default.
//   - (nil, err) — file exists but is malformed or fails validation. The
//     caller MUST surface this loudly (advisory verdict) rather than silently
//     fall back to probing — a broken manifest in a repo that has one is a
//     misconfiguration the operator must fix, not a missing-file scenario.
func loadQualityManifest(wsPath string) (*QualityManifest, error) {
	path := filepath.Join(wsPath, qualityManifestRelPath)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", qualityManifestRelPath, err)
	}

	var mf QualityManifest
	if err := yaml.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", qualityManifestRelPath, err)
	}
	if err := validateManifest(&mf); err != nil {
		return nil, fmt.Errorf("validate %s: %w", qualityManifestRelPath, err)
	}
	return &mf, nil
}

// validateManifest enforces the on-disk contract: a recognized runtime, at
// least one check, and per-check name+command both non-empty. It does NOT
// gate host-runtime against RICK_ALLOW_HOST_RUNTIME — that gating happens
// at execution time so an operator without the env can still inspect the
// manifest's intent in logs without rick refusing to load it.
func validateManifest(mf *QualityManifest) error {
	if mf.Runtime == "" {
		mf.Runtime = runtimeStack
	}
	switch mf.Runtime {
	case runtimeStack, runtimeHost:
	default:
		return fmt.Errorf("unknown runtime %q (expected %q or %q)", mf.Runtime, runtimeStack, runtimeHost)
	}
	if len(mf.Checks) == 0 {
		return fmt.Errorf("checks: must declare at least one check")
	}
	seen := map[string]bool{}
	for i, c := range mf.Checks {
		if c.Name == "" {
			return fmt.Errorf("checks[%d]: name is required", i)
		}
		if seen[c.Name] {
			return fmt.Errorf("checks[%d]: duplicate name %q", i, c.Name)
		}
		seen[c.Name] = true
		if len(c.Command) == 0 {
			return fmt.Errorf("checks[%d]: command is required for check %q", i, c.Name)
		}
		if c.Command[0] == "" {
			return fmt.Errorf("checks[%d]: command[0] (executable) cannot be empty for check %q", i, c.Name)
		}
	}
	return nil
}

// hostRuntimeAllowed reports whether RICK_ALLOW_HOST_RUNTIME=1 is set. Host
// runtime is opt-in because a malicious or buggy manifest can run arbitrary
// commands on the operator's host with their permissions; default-deny
// preserves the stack-VM isolation guarantee for any repo without explicit
// operator opt-in.
func hostRuntimeAllowed() bool {
	return os.Getenv("RICK_ALLOW_HOST_RUNTIME") == "1"
}

// checksFromManifest converts manifest checks into the qualityCheck shape the
// existing runner pipeline already understands. Label falls back to a joined
// command string when not set, so the operator-facing description always shows
// a runnable form.
func checksFromManifest(mf *QualityManifest) []qualityCheck {
	out := make([]qualityCheck, 0, len(mf.Checks))
	for _, c := range mf.Checks {
		label := c.Label
		if label == "" {
			label = joinCommand(c.Command)
		}
		out = append(out, qualityCheck{
			name:    c.Name,
			label:   label,
			command: c.Command,
		})
	}
	return out
}

// joinCommand produces a human-readable form of an argv slice without
// attempting full shell quoting — operators rarely need to copy-paste these
// (the manifest is the canonical source), and proper quoting would obscure
// the actual command structure for inspection.
func joinCommand(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(a)
	}
	return b.String()
}
