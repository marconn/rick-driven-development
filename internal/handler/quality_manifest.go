package handler

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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

// Manifests live in an operator-local config directory, NOT in the consumer
// repo. Rationale: the consumer repos (huli-api, ehr, practice-api, huli) are
// owned by other teams and shouldn't carry rick-specific config; rick owns
// the contract for how it drives them. Lookup precedence:
//
//  1. $RICK_QUALITY_MANIFESTS_DIR (operator override)
//  2. $XDG_CONFIG_HOME/rick/quality-manifests
//  3. $HOME/.config/rick/quality-manifests
//
// Within that directory, file lookup tries owner/name first, then bare name:
//
//  1. <dir>/<owner>/<name>.yaml — when git origin gives us both
//  2. <dir>/<name>.yaml         — fallback when only the workspace basename
//                                 is derivable, or as a cross-org alias
const (
	qualityManifestsEnv = "RICK_QUALITY_MANIFESTS_DIR"
	qualityManifestsRel = "rick/quality-manifests"
)

// QualityManifest is the on-disk schema for a per-repo manifest. The file
// declares which runtime to use and the exact commands rick will run; rick
// does not infer them.
type QualityManifest struct {
	// Runtime selects "stack" (default) or "host". A host-runtime manifest
	// is gated at execution time on RICK_ALLOW_HOST_RUNTIME=1 — see
	// hostRuntimeAllowed.
	Runtime runtimeKind `yaml:"runtime"`

	// Checks lists the commands rick will invoke, in order. They run
	// sequentially; failures collect and feed a single verdict.
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
	// failure descriptions. When omitted, falls back to a space-joined Command.
	Label string `yaml:"label,omitempty"`

	// Command is the argv passed to the runtime. First element is the
	// executable; remaining are args. No shell expansion is performed by rick.
	Command []string `yaml:"command"`
}

// repoIdentity is the (owner, name) pair extracted from a workspace's git
// origin URL, with a workspace-basename fallback for the no-remote case.
// owner can be empty when only the basename is derivable.
type repoIdentity struct {
	owner string
	name  string
}

// resolveQualityManifestsDir returns the local config directory rick reads
// quality manifests from. Empty string means rick can't locate a config dir
// (HOME and XDG_CONFIG_HOME both unset, override unset) — caller treats that
// the same as "no manifest", falling through to legacy probing.
func resolveQualityManifestsDir() string {
	if d := os.Getenv(qualityManifestsEnv); d != "" {
		return d
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, qualityManifestsRel)
	}
	if h := os.Getenv("HOME"); h != "" {
		return filepath.Join(h, ".config", qualityManifestsRel)
	}
	return ""
}

// detectRepoIdentity resolves (owner, name) for the workspace. Strategy:
//
//  1. Read the workspace's git origin URL — works for any rick-cloned
//     workspace because internal/workspace always sets up origin. Yields
//     both owner and name.
//  2. Fall back to the workspace directory's basename, stripped of the
//     `-rick-ws-<corrid>` suffix that internal/workspace appends in
//     isolated mode. Yields name only.
//
// Returns the zero repoIdentity when neither path produces a name. Callers
// treat that as "no manifest possible", same as a missing-file lookup.
func detectRepoIdentity(wsPath string) repoIdentity {
	if id, ok := repoFromGitOrigin(wsPath); ok {
		return id
	}
	if name := nameFromWorkspaceBasename(wsPath); name != "" {
		return repoIdentity{name: name}
	}
	return repoIdentity{}
}

// repoFromGitOrigin shells out to `git -C <wsPath> config --local
// remote.origin.url` and parses the result. Returns ok=false when git is
// missing, the workspace has no origin, or the URL has no parseable owner/name
// — every failure mode falls through to nameFromWorkspaceBasename.
//
// Why shell out instead of reading .git/config directly? git's URL handling
// (insteadOf rewrites, includeIf, worktrees) is more nuanced than a flat
// regex over the file; reusing the binary keeps us aligned with the same
// resolution any operator would see from their shell.
func repoFromGitOrigin(wsPath string) (repoIdentity, bool) {
	cmd := exec.Command("git", "-C", wsPath, "config", "--local", "remote.origin.url")
	out, err := cmd.Output()
	if err != nil {
		return repoIdentity{}, false
	}
	owner, name, ok := parseGitOriginURL(strings.TrimSpace(string(out)))
	if !ok {
		return repoIdentity{}, false
	}
	return repoIdentity{owner: owner, name: name}, true
}

// parseGitOriginURL extracts (owner, name) from a git remote URL. Handles
// the common forms:
//
//	git@github.com:owner/name.git
//	https://github.com/owner/name.git
//	ssh://git@github.com/owner/name.git
//	ssh://git@github.com:22/owner/name.git
//
// Strategy: strip a trailing .git, normalize ":" to "/" (handles SSH form
// and port-bearing ssh:// URLs uniformly), split on "/", take the last two
// non-empty segments. Returns ok=false when fewer than two segments survive.
func parseGitOriginURL(url string) (owner, name string, ok bool) {
	url = strings.TrimSpace(url)
	url = strings.TrimSuffix(url, ".git")
	if url == "" {
		return "", "", false
	}
	url = strings.ReplaceAll(url, ":", "/")
	var parts []string
	for _, p := range strings.Split(url, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return "", "", false
	}
	owner = parts[len(parts)-2]
	name = parts[len(parts)-1]
	if owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

// workspaceSuffix marks the boundary between the repo name and the
// correlation-derived suffix that internal/workspace appends in isolated
// mode (e.g. "huli-api-rick-ws-8ec9b848"). Stripping at this token recovers
// the repo name when no git remote is available.
const workspaceSuffix = "-rick-ws-"

// nameFromWorkspaceBasename returns the workspace basename with a trailing
// `-rick-ws-<corrid>` chunk removed. For non-isolated workspaces (no suffix),
// returns the basename unchanged. Empty input → empty output.
func nameFromWorkspaceBasename(wsPath string) string {
	base := filepath.Base(strings.TrimRight(wsPath, "/"))
	if base == "" || base == "." || base == "/" {
		return ""
	}
	if i := strings.Index(base, workspaceSuffix); i > 0 {
		return base[:i]
	}
	return base
}

// inRepoManifestRelPath is the in-workspace manifest location. It is
// gitignored in the consumer repo (kept off origin) but survives `cp -r`
// workspace provisioning, so a file the operator drops in their canonical
// checkout reaches every rick workspace cloned from it.
const inRepoManifestRelPath = ".rick/quality.yaml"

// loadLocalQualityManifest looks up the manifest for this workspace. Lookup
// precedence (first present file wins):
//
//  1. <wsPath>/.rick/quality.yaml — repo-local, gitignored. Carried into rick
//     workspaces by the cp -r provisioning in internal/workspace.
//  2. <manifestsDir>/<owner>/<name>.yaml — operator config, owner-scoped.
//  3. <manifestsDir>/<name>.yaml — operator config, cross-org alias.
//
// Empty manifestsDir disables (2) and (3); (1) still works. Undetectable
// repo identity disables (2) and (3) but again leaves (1) usable. A file
// that exists at any path but fails to parse/validate returns (nil, err)
// so detectQualityPlan can escalate as advisory rather than silently
// picking the next candidate.
func loadLocalQualityManifest(wsPath, manifestsDir string) (*QualityManifest, error) {
	if mf, err := loadManifestFile(filepath.Join(wsPath, inRepoManifestRelPath)); err != nil {
		return nil, err
	} else if mf != nil {
		return mf, nil
	}
	if manifestsDir == "" {
		return nil, nil
	}
	id := detectRepoIdentity(wsPath)
	if id.name == "" {
		return nil, nil
	}

	var candidates []string
	if id.owner != "" {
		candidates = append(candidates, filepath.Join(manifestsDir, id.owner, id.name+".yaml"))
	}
	candidates = append(candidates, filepath.Join(manifestsDir, id.name+".yaml"))

	for _, p := range candidates {
		mf, err := loadManifestFile(p)
		if err != nil {
			return nil, err
		}
		if mf != nil {
			return mf, nil
		}
	}
	return nil, nil
}

// loadManifestFile reads, parses, and validates a manifest at the given
// absolute path. Three return shapes:
//
//   - (manifest, nil) — file exists, parses cleanly, validates.
//   - (nil, nil) — file does not exist (ENOENT). Not an error.
//   - (nil, err) — file exists but malformed/invalid; caller MUST surface
//     loudly rather than fall back, since a broken manifest is a
//     misconfiguration the operator must fix.
func loadManifestFile(path string) (*QualityManifest, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var mf QualityManifest
	if err := yaml.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validateManifest(&mf); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
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
