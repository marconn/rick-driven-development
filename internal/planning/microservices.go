package planning

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MicroserviceMap maps microservice names to repository directory names.
// Loaded from AGENTS.md, vibe's microservices.txt, or a custom mapping file.
type MicroserviceMap struct {
	entries         map[string]string // microservice name -> repo dir name
	reposPath        string            // base path ($RICK_REPOS_PATH)
	platformContext string            // full AGENTS.md content for AI prompts
}

// NewMicroserviceMap creates a mapper with the given RICK_REPOS_PATH base.
func NewMicroserviceMap(reposPath string) *MicroserviceMap {
	return &MicroserviceMap{
		entries:  make(map[string]string),
		reposPath: reposPath,
	}
}

// LoadFromFile reads a microservices/platform context file.
// Supports:
//   - AGENTS.md / CLAUDE.md: Extracts repo names from "| repo-name |" table rows
//     and stores the full content as platform context for AI prompts.
//   - Simple list: one microservice name per line (name = repo dir)
//   - Mapping: "microservice=repodir" per line
//   - Vibe Jira format: "  10270: analytic" (ID: name)
func (m *MicroserviceMap) LoadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("microservices: read %s: %w", path, err)
	}
	content := string(data)

	// If it looks like markdown (AGENTS.md / CLAUDE.md), store full content
	// and extract repo names from the Repository Index table.
	if strings.Contains(content, "## ") || strings.HasSuffix(path, ".md") {
		m.platformContext = content
		m.extractReposFromMarkdown(content)
		return nil
	}

	// Fall back to line-by-line parsing for other formats.
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Vibe format: "10270: analytic"
		if len(line) > 0 && line[0] >= '0' && line[0] <= '9' {
			if idx := strings.Index(line, ": "); idx >= 0 {
				name := strings.TrimSpace(line[idx+2:])
				if name != "" {
					m.entries[name] = name
				}
			}
			continue
		}

		// Skip non-data lines
		if strings.HasPrefix(line, "Example") || strings.HasPrefix(line, "Available") || strings.HasPrefix(line, "python") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		name := strings.TrimSpace(parts[0])
		repo := name
		if len(parts) == 2 {
			repo = strings.TrimSpace(parts[1])
		}
		if name != "" {
			m.entries[name] = repo
		}
	}
	return scanner.Err()
}

// extractReposFromMarkdown parses "| repo-name | Category | ..." table rows.
func (m *MicroserviceMap) extractReposFromMarkdown(content string) {
	lines := strings.Split(content, "\n")
	inRepoTable := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect start of Repository Index table (heading, not ToC link).
		if strings.HasPrefix(trimmed, "#") && strings.Contains(trimmed, "Repository Index") {
			inRepoTable = true
			continue
		}
		// End of table on next heading or empty section
		if inRepoTable && strings.HasPrefix(trimmed, "## ") {
			break
		}

		if !inRepoTable || !strings.HasPrefix(trimmed, "|") {
			continue
		}
		// Skip header/separator rows
		if strings.Contains(trimmed, "----") || strings.Contains(trimmed, "Repository") {
			continue
		}

		// Parse "| repo-name | Category | ... |"
		parts := strings.Split(trimmed, "|")
		if len(parts) >= 3 {
			repo := strings.TrimSpace(parts[1])
			if repo != "" && !strings.Contains(repo, " ") {
				m.entries[repo] = repo
			}
		}
	}
}

// Names returns all known microservice names.
func (m *MicroserviceMap) Names() []string {
	names := make([]string, 0, len(m.entries))
	for name := range m.entries {
		names = append(names, name)
	}
	return names
}

// PlatformContext returns the full AGENTS.md content for AI prompts.
// Returns empty string if loaded from a non-markdown source.
func (m *MicroserviceMap) PlatformContext() string {
	return m.platformContext
}

// Add registers a microservice -> repo mapping.
func (m *MicroserviceMap) Add(microservice, repoDir string) {
	m.entries[microservice] = repoDir
}

// RepoPath returns the absolute path to a microservice's repository.
// Falls back to using the microservice name as the repo directory.
func (m *MicroserviceMap) RepoPath(microservice string) string {
	repo, ok := m.entries[microservice]
	if !ok {
		repo = microservice
	}
	return filepath.Join(m.reposPath, repo)
}

// Exists checks if a microservice's repo directory exists locally.
func (m *MicroserviceMap) Exists(microservice string) bool {
	info, err := os.Stat(m.RepoPath(microservice))
	return err == nil && info.IsDir()
}

// ListAvailable scans RICK_REPOS_PATH for directories that contain a .git folder (repos).
// Returns repo directory names, not full paths.
func (m *MicroserviceMap) ListAvailable() []string {
	entries, err := os.ReadDir(m.reposPath)
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		gitPath := filepath.Join(m.reposPath, e.Name(), ".git")
		if info, err := os.Stat(gitPath); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			repos = append(repos, e.Name())
		}
	}
	return repos
}

// ReposPath returns the base path for repos.
func (m *MicroserviceMap) ReposPath() string {
	return m.reposPath
}

// ResolveAll takes a list of microservice names and returns paths
// for those that exist locally, plus a list of missing ones.
func (m *MicroserviceMap) ResolveAll(microservices []string) (found map[string]string, missing []string) {
	found = make(map[string]string)
	for _, ms := range microservices {
		path := m.RepoPath(ms)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			found[ms] = path
		} else {
			missing = append(missing, ms)
		}
	}
	return found, missing
}
