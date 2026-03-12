package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the rick-agent configuration.
type Config struct {
	ServerURL string `json:"server_url"` // rick-server MCP endpoint
	Model     string `json:"model"`      // Gemini model name
	APIKey    string `json:"-"`          // resolved from environment
}

// DefaultConfig returns a Config with sensible defaults.
// Loads ~/.config/rick/env first so desktop launches pick up secrets
// without requiring the user to export them in their shell profile.
func DefaultConfig() Config {
	loadEnvFile()
	return Config{
		ServerURL: envOr("RICK_SERVER_URL", "http://localhost:8077/mcp"),
		Model:     envOr("RICK_MODEL", "gemini-2.5-pro"),
		APIKey:    resolveAPIKey(),
	}
}

// loadEnvFile reads KEY=VALUE lines from ~/.config/rick/env and sets them
// as environment variables. Existing env vars take precedence (not overwritten).
// Lines starting with # and empty lines are skipped.
func loadEnvFile() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	f, err := os.Open(filepath.Join(home, ".config", "rick", "env"))
	if err != nil {
		return
	}
	defer f.Close()

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
		value = strings.TrimSpace(value)
		// Don't overwrite — explicit env vars win (even if set to empty).
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
		}
	}
}

// Validate checks that required fields are set.
func (c Config) Validate() error {
	if c.ServerURL == "" {
		return fmt.Errorf("server URL is required (set RICK_SERVER_URL)")
	}
	if c.APIKey == "" {
		return fmt.Errorf("API key is required (set GOOGLE_API_KEY or GOOGLE_GENAI_API_KEY)")
	}
	return nil
}

func resolveAPIKey() string {
	if key := os.Getenv("GOOGLE_API_KEY"); key != "" {
		return key
	}
	return os.Getenv("GOOGLE_GENAI_API_KEY")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
