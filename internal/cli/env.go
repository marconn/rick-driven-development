package cli

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// loadConfigEnv reads KEY=VALUE lines from ~/.config/rick/env and exports any
// keys not already present in the process environment.
//
// Why the binary loads this itself rather than relying on systemd's
// EnvironmentFile: `rick serve` runs in several ways — the user-level unit
// (EnvironmentFile=-%h/.config/rick/env), the system unit (no EnvironmentFile
// at all), a bare `rick serve` from $PATH during dev — and a workflow that
// can't see RICK_REPOS_PATH never provisions a workspace and silently fails to
// run. Self-loading makes the env file authoritative regardless of launch path.
//
// Semantics mirror systemd's EnvironmentFile so the file behaves identically
// whether systemd or the binary reads it: a missing file is not an error
// (matching the leading `-`), `#` comments and blank lines are skipped, and one
// layer of matching surrounding quotes is stripped. Already-set variables win —
// an explicit shell export or a systemd `Environment=`/EnvironmentFile entry is
// never overwritten, so this is purely additive and safe to run unconditionally.
func loadConfigEnv() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	f, err := os.Open(filepath.Join(home, ".config", "rick", "env"))
	if err != nil {
		return // missing/unreadable file is non-fatal, like EnvironmentFile=-
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // explicit env (shell export, systemd) takes precedence
		}
		_ = os.Setenv(key, unquoteEnvValue(strings.TrimSpace(value)))
	}
}

// unquoteEnvValue strips a single layer of matching surrounding single or double
// quotes, mirroring how systemd parses EnvironmentFile values (so a path like
// RICK_REPOS_PATH="/home/me/repos" yields /home/me/repos, not the quoted form).
func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
