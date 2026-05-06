package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// makeNamedWorkspace creates a workspace dir whose basename matches a given
// repo name. Many tests use the workspace-basename fallback path of
// detectRepoIdentity (no git remote required), and that path keys off the
// dir's basename — so the test must control it explicitly. Returns the
// absolute path.
func makeNamedWorkspace(t *testing.T, name string) string {
	t.Helper()
	parent := t.TempDir()
	ws := filepath.Join(parent, name)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

// writeLocalManifest writes a manifest file under the local config dir
// using the owner/name path layout. owner="" writes the bare-name file
// (`<dir>/<name>.yaml`); a non-empty owner writes `<dir>/<owner>/<name>.yaml`.
func writeLocalManifest(t *testing.T, dir, owner, name, body string) string {
	t.Helper()
	var path string
	if owner == "" {
		path = filepath.Join(dir, name+".yaml")
	} else {
		path = filepath.Join(dir, owner, name+".yaml")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// initGitRepoWithOrigin initializes a fresh git repo at wsPath and sets the
// origin remote URL. Used by tests that exercise the git-origin discovery
// path of detectRepoIdentity.
func initGitRepoWithOrigin(t *testing.T, wsPath, originURL string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available; skipping git-origin path test")
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", originURL},
	} {
		cmd := exec.Command("git", append([]string{"-C", wsPath}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestParseGitOriginURL(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		wantOwner string
		wantName  string
		wantOK    bool
	}{
		{"ssh shorthand", "git@github.com:hulilabs/ehr.git", "hulilabs", "ehr", true},
		{"ssh shorthand no .git", "git@github.com:hulilabs/ehr", "hulilabs", "ehr", true},
		{"https form", "https://github.com/hulilabs/huli-api.git", "hulilabs", "huli-api", true},
		{"ssh url with port", "ssh://git@github.com:22/hulilabs/practice-api.git", "hulilabs", "practice-api", true},
		{"empty input", "", "", "", false},
		{"single segment", "owneronly", "", "", false},
		{"trailing slash", "https://github.com/hulilabs/ehr/", "hulilabs", "ehr", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, name, ok := parseGitOriginURL(tc.url)
			if ok != tc.wantOK {
				t.Fatalf("ok: want %v, got %v", tc.wantOK, ok)
			}
			if !ok {
				return
			}
			if owner != tc.wantOwner {
				t.Errorf("owner: want %q, got %q", tc.wantOwner, owner)
			}
			if name != tc.wantName {
				t.Errorf("name: want %q, got %q", tc.wantName, name)
			}
		})
	}
}

func TestNameFromWorkspaceBasename(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/path/to/ehr", "ehr"},
		{"/path/to/huli-api-rick-ws-8ec9b848", "huli-api"},
		{"/path/to/huli-api-rick-ws-", "huli-api"}, // suffix present, even if corr-id empty
		{"/path/to/repo-name/", "repo-name"},
		{"", ""},
		{"/", ""},
	}
	for _, tc := range cases {
		got := nameFromWorkspaceBasename(tc.path)
		if got != tc.want {
			t.Errorf("path=%q: want %q, got %q", tc.path, tc.want, got)
		}
	}
}

// TestDetectRepoIdentity_FromGitOrigin exercises the primary path: a workspace
// with a git remote yields both owner and name.
func TestDetectRepoIdentity_FromGitOrigin(t *testing.T) {
	ws := makeNamedWorkspace(t, "anything-rick-ws-deadbeef")
	initGitRepoWithOrigin(t, ws, "git@github.com:hulilabs/ehr.git")

	id := detectRepoIdentity(ws)
	if id.owner != "hulilabs" || id.name != "ehr" {
		t.Errorf("git-origin path: want hulilabs/ehr, got %q/%q", id.owner, id.name)
	}
}

// TestDetectRepoIdentity_BasenameFallback verifies the no-remote case: only
// the workspace basename is available, and the rick-ws- suffix is stripped.
func TestDetectRepoIdentity_BasenameFallback(t *testing.T) {
	ws := makeNamedWorkspace(t, "huli-api-rick-ws-8ec9b848")
	id := detectRepoIdentity(ws)
	if id.owner != "" {
		t.Errorf("owner: want empty (no git remote), got %q", id.owner)
	}
	if id.name != "huli-api" {
		t.Errorf("name: want huli-api, got %q", id.name)
	}
}

func TestResolveQualityManifestsDir(t *testing.T) {
	t.Run("env override wins", func(t *testing.T) {
		t.Setenv(qualityManifestsEnv, "/custom/path")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		t.Setenv("HOME", "/home/x")
		if got := resolveQualityManifestsDir(); got != "/custom/path" {
			t.Errorf("env override: want /custom/path, got %q", got)
		}
	})
	t.Run("XDG fallback", func(t *testing.T) {
		t.Setenv(qualityManifestsEnv, "")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		t.Setenv("HOME", "/home/x")
		want := filepath.Join("/xdg", qualityManifestsRel)
		if got := resolveQualityManifestsDir(); got != want {
			t.Errorf("XDG fallback: want %q, got %q", want, got)
		}
	})
	t.Run("HOME fallback", func(t *testing.T) {
		t.Setenv(qualityManifestsEnv, "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "/home/x")
		want := filepath.Join("/home/x", ".config", qualityManifestsRel)
		if got := resolveQualityManifestsDir(); got != want {
			t.Errorf("HOME fallback: want %q, got %q", want, got)
		}
	})
}

// TestLoadLocalQualityManifest_OwnerNamePath: the canonical case — owner and
// name resolved from git origin, manifest at <dir>/<owner>/<name>.yaml.
func TestLoadLocalQualityManifest_OwnerNamePath(t *testing.T) {
	ws := makeNamedWorkspace(t, "ws")
	initGitRepoWithOrigin(t, ws, "git@github.com:hulilabs/ehr.git")
	dir := t.TempDir()
	writeLocalManifest(t, dir, "hulilabs", "ehr", `
runtime: stack
checks:
  - name: test
    command: ["./run.sh", "test"]
`)

	mf, err := loadLocalQualityManifest(ws, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if mf == nil {
		t.Fatal("expected manifest, got nil")
	}
	if mf.Runtime != runtimeStack {
		t.Errorf("runtime: want stack, got %q", mf.Runtime)
	}
}

// TestLoadLocalQualityManifest_BareNameFallback: workspace lacks git remote;
// owner is unknown, lookup falls back to <dir>/<name>.yaml.
func TestLoadLocalQualityManifest_BareNameFallback(t *testing.T) {
	ws := makeNamedWorkspace(t, "huli-api-rick-ws-corrXYZ")
	dir := t.TempDir()
	writeLocalManifest(t, dir, "", "huli-api", `
checks:
  - name: test
    command: ["./run.sh", "test"]
`)

	mf, err := loadLocalQualityManifest(ws, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if mf == nil {
		t.Fatal("expected manifest at <dir>/huli-api.yaml, got nil")
	}
}

// TestLoadLocalQualityManifest_OwnerNameWinsOverBareName: when both files
// exist, the owner-scoped one is preferred. This guarantees an owner-scoped
// override always beats a cross-org alias.
func TestLoadLocalQualityManifest_OwnerNameWinsOverBareName(t *testing.T) {
	ws := makeNamedWorkspace(t, "ws")
	initGitRepoWithOrigin(t, ws, "git@github.com:hulilabs/ehr.git")
	dir := t.TempDir()
	writeLocalManifest(t, dir, "hulilabs", "ehr", `
runtime: stack
checks:
  - name: test
    command: ["scoped", "win"]
`)
	writeLocalManifest(t, dir, "", "ehr", `
runtime: stack
checks:
  - name: test
    command: ["bare", "lose"]
`)

	mf, err := loadLocalQualityManifest(ws, dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := mf.Checks[0].Command[0]; got != "scoped" {
		t.Errorf("owner-scoped manifest must win; got command[0]=%q", got)
	}
}

// TestLoadLocalQualityManifest_NoFile: no matching file in the dir → (nil, nil).
// Callers fall through to legacy probing.
func TestLoadLocalQualityManifest_NoFile(t *testing.T) {
	ws := makeNamedWorkspace(t, "ehr-rick-ws-x")
	mf, err := loadLocalQualityManifest(ws, t.TempDir())
	if err != nil {
		t.Fatalf("missing file must not error, got: %v", err)
	}
	if mf != nil {
		t.Errorf("missing file must return nil, got: %+v", mf)
	}
}

// TestLoadLocalQualityManifest_EmptyDirIsNoOp: manifestsDir="" disables
// manifest lookup entirely. Used by tests of the legacy fallback path
// and as a safety valve when HOME/XDG are unset.
func TestLoadLocalQualityManifest_EmptyDirIsNoOp(t *testing.T) {
	ws := makeNamedWorkspace(t, "ehr")
	mf, err := loadLocalQualityManifest(ws, "")
	if err != nil {
		t.Fatalf("empty manifestsDir must not error: %v", err)
	}
	if mf != nil {
		t.Errorf("empty manifestsDir must return nil")
	}
}

// TestLoadLocalQualityManifest_MalformedSurfacesError: a present-but-broken
// manifest must NOT fall through silently — the error must propagate so
// detectQualityPlan can escalate as advisory.
func TestLoadLocalQualityManifest_MalformedSurfacesError(t *testing.T) {
	ws := makeNamedWorkspace(t, "ehr")
	dir := t.TempDir()
	writeLocalManifest(t, dir, "", "ehr", `
checks:
  - name: test
    command: [unterminated
`)

	_, err := loadLocalQualityManifest(ws, dir)
	if err == nil {
		t.Fatal("expected parse error from malformed manifest")
	}
}

// --- loadManifestFile validation tests --- //
//
// These exercise the YAML parsing + schema validation in isolation,
// independent of repo-identity resolution. They write directly to a path.

func writeManifestFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quality.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadManifestFile_Missing(t *testing.T) {
	mf, err := loadManifestFile(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if mf != nil {
		t.Errorf("missing file must return nil")
	}
}

func TestLoadManifestFile_ValidStack(t *testing.T) {
	path := writeManifestFile(t, `
runtime: stack
checks:
  - name: lint
    command: ["./run.sh", "lint"]
  - name: test
    label: "./run.sh up && ./run.sh test"
    command: ["bash", "-c", "./run.sh up && ./run.sh test"]
`)
	mf, err := loadManifestFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if mf.Runtime != runtimeStack {
		t.Errorf("runtime: want stack, got %q", mf.Runtime)
	}
	if len(mf.Checks) != 2 {
		t.Fatalf("want 2 checks, got %d", len(mf.Checks))
	}
}

func TestLoadManifestFile_RuntimeDefault(t *testing.T) {
	path := writeManifestFile(t, `
checks:
  - name: test
    command: ["./run.sh", "test"]
`)
	mf, err := loadManifestFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if mf.Runtime != runtimeStack {
		t.Errorf("default runtime: want stack, got %q", mf.Runtime)
	}
}

func TestLoadManifestFile_RejectsUnknownRuntime(t *testing.T) {
	path := writeManifestFile(t, `
runtime: container
checks:
  - name: test
    command: ["./run.sh", "test"]
`)
	if _, err := loadManifestFile(path); err == nil {
		t.Fatal("expected validation error for unknown runtime")
	}
}

func TestLoadManifestFile_RejectsEmptyChecks(t *testing.T) {
	path := writeManifestFile(t, `
runtime: stack
checks: []
`)
	if _, err := loadManifestFile(path); err == nil {
		t.Fatal("expected validation error for empty checks")
	}
}

func TestLoadManifestFile_RejectsDuplicateNames(t *testing.T) {
	path := writeManifestFile(t, `
checks:
  - name: test
    command: ["./run.sh", "test"]
  - name: test
    command: ["./run.sh", "test", "again"]
`)
	if _, err := loadManifestFile(path); err == nil {
		t.Fatal("expected validation error for duplicate check names")
	}
}

func TestLoadManifestFile_RejectsEmptyCommand(t *testing.T) {
	path := writeManifestFile(t, `
checks:
  - name: test
    command: []
`)
	if _, err := loadManifestFile(path); err == nil {
		t.Fatal("expected validation error for empty command")
	}
}

func TestLoadManifestFile_RejectsMalformedYAML(t *testing.T) {
	path := writeManifestFile(t, "checks:\n  - name: test\n    command: [unterminated\n")
	if _, err := loadManifestFile(path); err == nil {
		t.Fatal("expected parse error for malformed YAML")
	}
}

// --- detectQualityPlan integration tests --- //

// TestDetectQualityPlan_ManifestPresentTakesPrecedence: a local manifest beats
// the legacy run.sh probe. Validates the migration story — adding a manifest
// immediately overrides the default heuristic without removing run.sh.
func TestDetectQualityPlan_ManifestPresentTakesPrecedence(t *testing.T) {
	ws := makeNamedWorkspace(t, "ehr-rick-ws-x")
	if err := os.WriteFile(filepath.Join(ws, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeLocalManifest(t, dir, "", "ehr", `
runtime: host
checks:
  - name: test
    command: ["go", "test", "./..."]
`)

	plan, err := detectQualityPlan(ws, dir)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if plan.runner != runnerManifest {
		t.Errorf("runner: want manifest, got %q", plan.runner)
	}
	if plan.runtime != runtimeHost {
		t.Errorf("runtime: want host, got %q", plan.runtime)
	}
	if len(plan.checks) != 1 || plan.checks[0].name != "test" {
		t.Errorf("checks: want [test], got %+v", plan.checks)
	}
}

// TestDetectQualityPlan_FallbackToProbe: no manifest, run.sh present →
// runs the legacy probing path with stack runtime. Backward-compat guarantee.
func TestDetectQualityPlan_FallbackToProbe(t *testing.T) {
	ws := makeNamedWorkspace(t, "some-repo")
	if err := os.WriteFile(filepath.Join(ws, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	plan, err := detectQualityPlan(ws, t.TempDir()) // empty dir, no manifest
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if plan.runner != runnerRunSh {
		t.Errorf("runner: want run.sh, got %q", plan.runner)
	}
	if plan.runtime != runtimeStack {
		t.Errorf("runtime: want stack, got %q", plan.runtime)
	}
	if len(plan.checks) != 2 {
		t.Errorf("checks: want 2 (lint+test), got %d", len(plan.checks))
	}
}

// TestDetectQualityPlan_NoDriver: empty workspace, no manifest, no runner.
func TestDetectQualityPlan_NoDriver(t *testing.T) {
	ws := makeNamedWorkspace(t, "empty")
	plan, err := detectQualityPlan(ws, t.TempDir())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if plan.runner != runnerNone {
		t.Errorf("runner: want none, got %q", plan.runner)
	}
}

// TestDetectQualityPlan_BrokenManifestSurfacesError: malformed manifest
// returns an error so Handle can escalate as advisory.
func TestDetectQualityPlan_BrokenManifestSurfacesError(t *testing.T) {
	ws := makeNamedWorkspace(t, "ehr-rick-ws-x")
	if err := os.WriteFile(filepath.Join(ws, "run.sh"), []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeLocalManifest(t, dir, "", "ehr", "runtime: bogus\nchecks: []\n")

	if _, err := detectQualityPlan(ws, dir); err == nil {
		t.Fatal("expected error from broken manifest, got nil")
	}
}

// --- host-runtime integration through Handle --- //

// TestQualityGate_HostRuntimeGated: manifest declares host but env is unset
// → advisory verdict. Default-deny on host runtime is the safety boundary.
func TestQualityGate_HostRuntimeGated(t *testing.T) {
	t.Setenv("RICK_ALLOW_HOST_RUNTIME", "")
	ws := makeNamedWorkspace(t, "tinyrepo-rick-ws-corr1")
	dir := t.TempDir()
	writeLocalManifest(t, dir, "", "tinyrepo", `
runtime: host
checks:
  - name: test
    command: ["true"]
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: ws, Branch: "test"})
	store.correlationEvents["corr-host-gated"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-host-gated"),
	}
	h := &QualityGateHandler{
		store:        store,
		name:         "quality-gate",
		stackBin:     "stack",
		timeout:      300,
		manifestsDir: dir,
		logger:       slog.Default(),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-host-gated")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if !vp.Advisory {
		t.Errorf("host-runtime gating must produce advisory verdict, got non-advisory")
	}
	if !strings.Contains(vp.Summary, "host_runtime_not_allowed") {
		t.Errorf("summary should name the gating reason; got %q", vp.Summary)
	}
}

// TestQualityGate_HostRuntime_Executes: env set + always-passing host command
// → pass verdict, no stack VM spawned.
func TestQualityGate_HostRuntime_Executes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host runtime test uses /bin/true; not portable to windows")
	}
	t.Setenv("RICK_ALLOW_HOST_RUNTIME", "1")
	ws := makeNamedWorkspace(t, "tinyrepo-rick-ws-corr2")
	dir := t.TempDir()
	writeLocalManifest(t, dir, "", "tinyrepo", `
runtime: host
checks:
  - name: test
    command: ["true"]
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: ws, Branch: "test"})
	store.correlationEvents["corr-host-ok"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-host-ok"),
	}
	// Bogus stackBin proves host runtime never invokes it.
	h := &QualityGateHandler{
		store:        store,
		name:         "quality-gate",
		stackBin:     "/nonexistent/stack",
		timeout:      300,
		manifestsDir: dir,
		logger:       slog.Default(),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-host-ok")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictPass)
}

// TestQualityGate_HostRuntime_ExecMissing: nonexistent executable produces
// an advisory infrastructure verdict (host_executable_missing), not a "test
// failed" regression report. The developer cannot fix a missing binary.
func TestQualityGate_HostRuntime_ExecMissing(t *testing.T) {
	t.Setenv("RICK_ALLOW_HOST_RUNTIME", "1")
	ws := makeNamedWorkspace(t, "tinyrepo-rick-ws-corr3")
	dir := t.TempDir()
	writeLocalManifest(t, dir, "", "tinyrepo", `
runtime: host
checks:
  - name: test
    command: ["this-binary-does-not-exist-anywhere"]
`)

	store := newMockStore()
	wsPayload := event.MustMarshal(event.WorkspaceReadyPayload{Path: ws, Branch: "test"})
	store.correlationEvents["corr-missing-bin"] = []event.Envelope{
		event.New(event.WorkspaceReady, 1, wsPayload).WithCorrelation("corr-missing-bin"),
	}
	h := &QualityGateHandler{
		store:        store,
		name:         "quality-gate",
		stackBin:     "stack",
		timeout:      300,
		manifestsDir: dir,
		logger:       slog.Default(),
	}
	triggerEvt := event.New(event.PersonaCompleted, 1, nil).WithCorrelation("corr-missing-bin")

	got, err := h.Handle(context.Background(), triggerEvt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertVerdictOutcome(t, got[0], event.VerdictFail)

	var vp event.VerdictPayload
	if err := json.Unmarshal(got[0].Payload, &vp); err != nil {
		t.Fatal(err)
	}
	if !vp.Advisory {
		t.Errorf("missing executable must produce advisory verdict (operator action required)")
	}
	if len(vp.Issues) == 0 || vp.Issues[0].Category != "infrastructure" {
		t.Errorf("issue category: want infrastructure, got %+v", vp.Issues)
	}
}

// --- parser regression tests --- //

// TestParseStackNDJSON_LargeRunEnvelope is the regression for the 2026-05-06
// huli-api silent failure. Stack inlines 100KB+ of docker-pull noise into
// the "run" envelope's stderr field; bufio.Scanner's default 64KB buffer
// silently dropped the line and we fell back to the empty "create" envelope.
// This test feeds a 200KB envelope (well above 64KB, well below 16MB) and
// verifies the run envelope's payload survives intact.
func TestParseStackNDJSON_LargeRunEnvelope(t *testing.T) {
	bigStderr := strings.Repeat("docker-pull-noise ", 12*1024) // ~204 KiB
	runLine, err := json.Marshal(stackRunResult{
		Action:   "run",
		Status:   "success",
		ExitCode: 1,
		Output:   "",
		Stderr:   bigStderr + "\nno such service: code\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	ndjson := []byte(`{"action":"create","status":"success"}` + "\n" +
		string(runLine) + "\n" +
		`{"action":"destroy","status":"success"}` + "\n")

	got, ok := parseStackNDJSON(ndjson)
	if !ok {
		t.Fatal("parseStackNDJSON returned !ok for a 200KB run envelope; the 64KB scanner limit is back")
	}
	if got.Action != "run" {
		t.Errorf("action: want run, got %q (parser fell back to create/destroy)", got.Action)
	}
	if got.ExitCode != 1 {
		t.Errorf("exit_code: want 1, got %d", got.ExitCode)
	}
	if !strings.Contains(got.Stderr, "no such service: code") {
		t.Errorf("stderr lost the diagnostic; first 200 chars: %q", truncateForLog(got.Stderr, 200))
	}
}

// TestParseStackNDJSON_OversizedLine: a single NDJSON line beyond our 16 MiB
// ceiling is treated as a hard parse failure (returns false). Better to
// surface parse_error to the operator than silently fall back to a partial
// parse that loses the run envelope.
//
// The test prepends a small envelope before the oversized one so the data
// is NDJSON (multi-line). A single-JSON-document fast path in parseStackNDJSON
// would otherwise short-circuit before the scanner runs, hiding the size
// limit. Production stack output is always multi-line.
func TestParseStackNDJSON_OversizedLine(t *testing.T) {
	bigStderr := strings.Repeat("x", 17*1024*1024)
	bigLine := fmt.Sprintf(`{"action":"run","status":"success","stderr":%q}`, bigStderr)
	ndjson := []byte(`{"action":"create","status":"success"}` + "\n" + bigLine + "\n")

	if _, ok := parseStackNDJSON(ndjson); ok {
		t.Fatal("oversized line must produce parseOK=false; partial parses are not allowed")
	}
}

// truncateForLog returns the first n chars of s for compact test failure
// messages.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
