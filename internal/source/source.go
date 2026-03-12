package source

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type SourceType string

const (
	Raw    SourceType = "raw"
	File   SourceType = "file"
	Jira   SourceType = "jira"
	GitHub SourceType = "github"
)

type Source struct {
	Type      SourceType
	Reference string
	Content   string
}

type Resolver struct{}

func NewResolver() *Resolver {
	return &Resolver{}
}

func (r *Resolver) Resolve(_ context.Context, input string) (*Source, error) {
	switch {
	case strings.HasPrefix(input, "file:"):
		return resolveFile(input)
	case strings.HasPrefix(input, "jira:"):
		return resolveJira(input), nil
	case strings.HasPrefix(input, "gh:"):
		return resolveGitHub(input)
	default:
		return &Source{Type: Raw, Content: input}, nil
	}
}

func resolveFile(input string) (*Source, error) {
	path := strings.TrimPrefix(input, "file:")
	if path == "" {
		return nil, fmt.Errorf("source: file path is empty")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("source: reading file %q: %w", path, err)
	}

	return &Source{
		Type:      File,
		Reference: path,
		Content:   string(data),
	}, nil
}

func resolveJira(input string) *Source {
	key := strings.TrimPrefix(input, "jira:")
	return &Source{
		Type:      Jira,
		Reference: key,
		Content:   "jira:" + key,
	}
}

func resolveGitHub(input string) (*Source, error) {
	ref := strings.TrimPrefix(input, "gh:")

	// Parse owner/repo#number
	parts := strings.SplitN(ref, "#", 2)
	if len(parts) != 2 {
		return &Source{Type: GitHub, Reference: ref, Content: ref}, nil
	}
	repo, number := parts[0], parts[1]

	// Fetch issue content via gh CLI
	out, err := exec.Command("gh", "issue", "view", number, "--repo", repo, "--json", "title,body").Output()
	if err != nil {
		return nil, fmt.Errorf("source: fetch github issue %s: %w", ref, err)
	}

	var issue struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("source: parse github issue %s: %w", ref, err)
	}

	content := fmt.Sprintf("GitHub Issue: %s\n\n%s", issue.Title, issue.Body)
	return &Source{
		Type:      GitHub,
		Reference: ref,
		Content:   content,
	}, nil
}
