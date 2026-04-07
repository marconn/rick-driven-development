package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	// Clear env to get true defaults.
	t.Setenv("RICK_SERVER_URL", "")
	t.Setenv("RICK_MODEL", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GOOGLE_GENAI_API_KEY", "")

	cfg := DefaultConfig()

	if cfg.ServerURL != "http://localhost:58077/mcp" {
		t.Errorf("expected default server URL http://localhost:58077/mcp, got %s", cfg.ServerURL)
	}
	if cfg.Model != "gemini-2.5-pro" {
		t.Errorf("expected default model gemini-2.5-pro, got %s", cfg.Model)
	}
	if cfg.APIKey != "" {
		t.Errorf("expected empty API key with no env vars, got %s", cfg.APIKey)
	}
}

func TestDefaultConfigFromEnv(t *testing.T) {
	t.Setenv("RICK_SERVER_URL", "http://custom:9999/mcp")
	t.Setenv("RICK_MODEL", "gemini-2.0-flash")
	t.Setenv("GOOGLE_API_KEY", "test-api-key")

	cfg := DefaultConfig()

	if cfg.ServerURL != "http://custom:9999/mcp" {
		t.Errorf("expected custom server URL, got %s", cfg.ServerURL)
	}
	if cfg.Model != "gemini-2.0-flash" {
		t.Errorf("expected gemini-2.0-flash, got %s", cfg.Model)
	}
	if cfg.APIKey != "test-api-key" {
		t.Errorf("expected test-api-key, got %s", cfg.APIKey)
	}
}

func TestResolveAPIKeyPriority(t *testing.T) {
	// GOOGLE_API_KEY takes precedence over GOOGLE_GENAI_API_KEY.
	t.Setenv("GOOGLE_API_KEY", "primary-key")
	t.Setenv("GOOGLE_GENAI_API_KEY", "fallback-key")

	key := resolveAPIKey()
	if key != "primary-key" {
		t.Errorf("expected GOOGLE_API_KEY to take precedence, got %s", key)
	}
}

func TestResolveAPIKeyFallback(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GOOGLE_GENAI_API_KEY", "fallback-key")

	key := resolveAPIKey()
	if key != "fallback-key" {
		t.Errorf("expected GOOGLE_GENAI_API_KEY fallback, got %s", key)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "valid",
			cfg: Config{
				ServerURL: "http://localhost:58077/mcp",
				Model:     "gemini-2.5-pro",
				APIKey:    "test-key",
			},
			wantErr: false,
		},
		{
			name: "missing server URL",
			cfg: Config{
				ServerURL: "",
				Model:     "gemini-2.5-pro",
				APIKey:    "test-key",
			},
			wantErr: true,
		},
		{
			name: "missing API key",
			cfg: Config{
				ServerURL: "http://localhost:58077/mcp",
				Model:     "gemini-2.5-pro",
				APIKey:    "",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEnvOr(t *testing.T) {
	key := "TEST_ENV_OR_" + t.Name()

	// Unset — should return fallback.
	os.Unsetenv(key)
	if got := envOr(key, "default"); got != "default" {
		t.Errorf("expected default, got %s", got)
	}

	// Set — should return env value.
	t.Setenv(key, "custom")
	if got := envOr(key, "default"); got != "custom" {
		t.Errorf("expected custom, got %s", got)
	}
}

// --- loadEnvFile parsing tests ---

// writeEnvFile is a helper that writes lines to ~/.config/rick/env under a temp HOME.
func writeEnvFile(t *testing.T, homeDir, content string) {
	t.Helper()
	rickDir := filepath.Join(homeDir, ".config", "rick")
	if err := os.MkdirAll(rickDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rickDir, "env"), []byte(content), 0o644); err != nil {
		t.Fatalf("write env file: %v", err)
	}
}

func TestLoadEnvFileKeyValueLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	writeEnvFile(t, dir, "MY_TEST_KEY_ABC=myvalue\n")

	os.Unsetenv("MY_TEST_KEY_ABC")
	loadEnvFile()

	if got := os.Getenv("MY_TEST_KEY_ABC"); got != "myvalue" {
		t.Errorf("expected 'myvalue', got %q", got)
	}
}

func TestLoadEnvFileSkipsComments(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	writeEnvFile(t, dir, "# this is a comment\nMY_TEST_NOT_COMMENT=real\n")

	os.Unsetenv("MY_TEST_NOT_COMMENT")
	loadEnvFile()

	if got := os.Getenv("MY_TEST_NOT_COMMENT"); got != "real" {
		t.Errorf("expected 'real', got %q", got)
	}
}

func TestLoadEnvFileSkipsEmptyLines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	writeEnvFile(t, dir, "\n\nMY_TEST_AFTER_EMPTY=after\n\n")

	os.Unsetenv("MY_TEST_AFTER_EMPTY")
	loadEnvFile()

	if got := os.Getenv("MY_TEST_AFTER_EMPTY"); got != "after" {
		t.Errorf("expected 'after', got %q", got)
	}
}

func TestLoadEnvFileSkipsLinesWithoutEquals(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// "JUSTKEY" has no '=' — should be silently ignored.
	writeEnvFile(t, dir, "JUSTKEY\nMY_TEST_VALID_PAIR=yes\n")

	os.Unsetenv("JUSTKEY")
	os.Unsetenv("MY_TEST_VALID_PAIR")
	loadEnvFile()

	// JUSTKEY must NOT be set (no '=' to split on).
	if got := os.Getenv("JUSTKEY"); got != "" {
		t.Errorf("expected JUSTKEY to be unset, got %q", got)
	}
	if got := os.Getenv("MY_TEST_VALID_PAIR"); got != "yes" {
		t.Errorf("expected 'yes', got %q", got)
	}
}

func TestLoadEnvFileTrimWhitespace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	writeEnvFile(t, dir, "  MY_TEST_TRIMMED  =  trimmedvalue  \n")

	os.Unsetenv("MY_TEST_TRIMMED")
	loadEnvFile()

	if got := os.Getenv("MY_TEST_TRIMMED"); got != "trimmedvalue" {
		t.Errorf("expected 'trimmedvalue', got %q", got)
	}
}

func TestLoadEnvFileDoesNotOverwriteExistingVars(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	writeEnvFile(t, dir, "MY_TEST_PREEXISTING=from_file\n")

	// Pre-set the var — loadEnvFile must NOT overwrite it.
	t.Setenv("MY_TEST_PREEXISTING", "original")
	loadEnvFile()

	if got := os.Getenv("MY_TEST_PREEXISTING"); got != "original" {
		t.Errorf("expected 'original' (not overwritten), got %q", got)
	}
}

func TestLoadEnvFileMissingFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// No env file created — loadEnvFile must not panic or error.
	loadEnvFile() // should be a no-op
}

func TestDefaultConfigReadsEnvFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Write env file with all three config keys.
	writeEnvFile(t, dir,
		"RICK_SERVER_URL=http://from-file:58077/mcp\nRICK_MODEL=gemini-from-file\nGOOGLE_API_KEY=key-from-file\n",
	)

	// Clear the env vars so loadEnvFile can set them.
	os.Unsetenv("RICK_SERVER_URL")
	os.Unsetenv("RICK_MODEL")
	os.Unsetenv("GOOGLE_API_KEY")
	os.Unsetenv("GOOGLE_GENAI_API_KEY")

	cfg := DefaultConfig()

	if cfg.ServerURL != "http://from-file:58077/mcp" {
		t.Errorf("expected ServerURL from env file, got %s", cfg.ServerURL)
	}
	if cfg.Model != "gemini-from-file" {
		t.Errorf("expected Model from env file, got %s", cfg.Model)
	}
	if cfg.APIKey != "key-from-file" {
		t.Errorf("expected APIKey from env file, got %s", cfg.APIKey)
	}
}
