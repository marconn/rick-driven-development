package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rickDir := filepath.Join(home, ".config", "rick")
	if err := os.MkdirAll(rickDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Synthetic keys (RICK_TEST_*) so the assertions don't collide with vars the
	// developer's shell may already export (e.g. RICK_REPOS_PATH) — those would
	// be preserved by existing-wins and mask the file values the test injects.
	content := "" +
		"# comment line\n" +
		"\n" +
		"RICK_TEST_PLAIN=/home/me/repos\n" +
		"RICK_TEST_DQUOTE=\"gemini-2.5-pro\"\n" +
		"RICK_TEST_SQUOTE='me@example.com'\n" +
		"RICK_TEST_EXISTING=from-file\n" +
		"   RICK_TEST_TRIMMED = spaced \n" +
		"NO_EQUALS_LINE\n"
	if err := os.WriteFile(filepath.Join(rickDir, "env"), []byte(content), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	// An explicit env var must win over the file.
	t.Setenv("RICK_TEST_EXISTING", "from-shell")

	loadConfigEnv()

	cases := map[string]string{
		"RICK_TEST_PLAIN":    "/home/me/repos", // plain value
		"RICK_TEST_DQUOTE":   "gemini-2.5-pro", // double quotes stripped
		"RICK_TEST_SQUOTE":   "me@example.com", // single quotes stripped
		"RICK_TEST_EXISTING": "from-shell",     // explicit env preserved
		"RICK_TEST_TRIMMED":  "spaced",         // key/value trimmed
	}
	for k, want := range cases {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestLoadConfigEnv_MissingFileIsNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no .config/rick/env exists
	// Must not panic or set anything; absence mirrors EnvironmentFile=-.
	loadConfigEnv()
	if _, ok := os.LookupEnv("RICK_TEST_SENTINEL_UNSET"); ok {
		t.Fatal("unexpected env var set from a missing file")
	}
}
