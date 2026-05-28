package handler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestExtractRemovedSymbols(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want []string
	}{
		{
			name: "renamed exported symbol surfaces",
			diff: "+++ b/user.go\n" +
				"-func FetchUser(id string) (*User, error) {\n" +
				"+func FetchAccount(id string) (*Account, error) {\n",
			want: []string{"FetchUser"}, // User/Account are single-capital → filtered
		},
		{
			name: "edited in place is not a rename",
			diff: "+++ b/user.go\n" +
				"-\tFetchUser(ctx, oldArg)\n" +
				"+\tFetchUser(ctx, newArg)\n",
			want: nil, // FetchUser present on both sides
		},
		{
			name: "deleted screaming snake const",
			diff: "+++ b/config.go\n" +
				"-const MAX_RETRIES = 3\n" +
				"+const maxAttempts = 3\n",
			want: []string{"MAX_RETRIES"},
		},
		{
			name: "removed path literal",
			diff: "+++ b/router.go\n" +
				"-mux.Handle(\"internal/handler/legacy\", h)\n" +
				"+mux.Handle(\"internal/handler/v2\", h)\n",
			want: []string{"internal/handler/legacy"},
		},
		{
			name: "lowercase and short tokens filtered",
			diff: "+++ b/x.go\n" +
				"-\tfoo := bar(baz)\n" +
				"+\tqux := bar(baz)\n",
			want: nil,
		},
		{
			name: "single-capital common types filtered",
			diff: "+++ b/x.go\n" +
				"-\treturn Error{}, String(\"x\")\n" +
				"+\treturn nil, \"x\"\n",
			want: nil, // Error, String each have only one uppercase letter
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRemovedSymbols(tc.diff)
			sort.Strings(tc.want)
			if !equalStrings(got, tc.want) {
				t.Errorf("extractRemovedSymbols() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsCandidateSymbol(t *testing.T) {
	tests := map[string]bool{
		"FetchUser":            true,
		"WorkspaceReady":       true,
		"PRStaleReference":     true,
		"HTTPClient":           true,
		"UserID":               true,
		"MAX_RETRIES":          true,
		"RICK_ENABLE_X_SWEEP":  true,
		"internal/handler":     true,
		"context.enrichment":   true,
		"Error":                false, // one uppercase
		"String":               false, // one uppercase
		"Handler":              false, // one uppercase
		"id":                   false, // lowercase + short
		"Get":                  false, // too short
		"foobar":               false, // lowercase
	}
	for tok, want := range tests {
		if got := isCandidateSymbol(tok); got != want {
			t.Errorf("isCandidateSymbol(%q) = %v, want %v", tok, got, want)
		}
	}
}

func TestIsCommentLine(t *testing.T) {
	tests := map[string]bool{
		"// a Go comment":     true,
		"   # shell/py":       true,
		"* javadoc/block":     true,
		"/* c-style":          true,
		"-- sql/lua":          true,
		"; ini":               true,
		"<!-- html":           true,
		`""" python docstring`: true,
		"x := FetchUser()":    false,
		"return nil":          false,
	}
	for line, want := range tests {
		if got := isCommentLine(line); got != want {
			t.Errorf("isCommentLine(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestIsStaleRefDocFile(t *testing.T) {
	tests := map[string]bool{
		"README.md":              true,
		"docs/CLAUDE.md":         true,
		"notes.rst":              true,
		"guide.adoc":             true,
		"changelog.txt":          true,
		"internal/handler/x.go":  false,
		"Makefile":               false,
		"script.sh":              false,
	}
	for path, want := range tests {
		if got := isStaleRefDocFile(path); got != want {
			t.Errorf("isStaleRefDocFile(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParseGitGrepLine(t *testing.T) {
	f, ok := parseGitGrepLine("docs/README.md:42:  See FetchUser for details")
	if !ok || f.File != "docs/README.md" || f.Line != 42 || f.Snippet != "See FetchUser for details" {
		t.Errorf("parseGitGrepLine good case = %+v ok=%v", f, ok)
	}
	for _, bad := range []string{"no-colons-here", "path-only:", "path:notanumber:x", "path:0:x"} {
		if _, ok := parseGitGrepLine(bad); ok {
			t.Errorf("parseGitGrepLine(%q) should fail", bad)
		}
	}
}

func TestFormatStaleReferences(t *testing.T) {
	out := formatStaleReferences([]staleRefFinding{
		{Symbol: "FetchUser", File: "README.md", Line: 12, Snippet: "see FetchUser"},
	}, false)
	for _, want := range []string{"advisory", "`FetchUser`", "`README.md:12`", "see FetchUser"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatStaleReferences missing %q in:\n%s", want, out)
		}
	}
}

// TestSweepFiltersByDocAndComment is a git-grep integration test: a renamed
// symbol referenced in a doc file and a code comment must surface, while the
// same symbol in unchanged *executable* code must not (that's a compile/
// semantic concern other layers own, not doc rot).
func TestSweepFiltersByDocAndComment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	writeRepoFile(t, dir, "README.md", "Architecture: FetchUser loads the account.\n")
	writeRepoFile(t, dir, "notes.go", "package x\n\n// FetchUser is the legacy entrypoint.\nfunc y() {}\n")
	writeRepoFile(t, dir, "live.go", "package x\n\nfunc z() { _ = FetchUser() }\n")
	gitRun(t, dir, "add", ".")

	h := &PRStaleReferenceHandler{}
	findings := h.sweep(context.Background(), dir, []string{"FetchUser"}, map[string]struct{}{})

	gotFiles := map[string]bool{}
	for _, f := range findings {
		gotFiles[f.File] = true
	}
	if !gotFiles["README.md"] {
		t.Error("expected README.md (doc file) hit")
	}
	if !gotFiles["notes.go"] {
		t.Error("expected notes.go (comment line) hit")
	}
	if gotFiles["live.go"] {
		t.Error("live.go executable-code reference must be filtered out")
	}
}

// TestSweepExcludesChangedFiles confirms the grep exclusion set is honored.
func TestSweepExcludesChangedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	writeRepoFile(t, dir, "README.md", "FetchUser is here.\n")
	gitRun(t, dir, "add", ".")

	h := &PRStaleReferenceHandler{}
	findings := h.sweep(context.Background(), dir, []string{"FetchUser"}, map[string]struct{}{"README.md": {}})
	if len(findings) != 0 {
		t.Errorf("changed file should be excluded, got %+v", findings)
	}
}

// --- helpers ---

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "test")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
