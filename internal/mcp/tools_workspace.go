package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/workspace"
)

func (s *Server) registerWorkspaceTools() {

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workspace_setup",
			Description: "Create an isolated local clone of a repository under $RICK_REPOS_PATH. Checks out a branch from origin/<base>. Returns the workspace path. ALWAYS use this before running code-writing jobs to prevent collisions.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"repo": map[string]any{
						"type":        "string",
						"description": "Repository name under $RICK_REPOS_PATH (e.g., 'backend', 'frontend').",
					},
					"ticket": map[string]any{
						"type":        "string",
						"description": "Jira ticket ID for the branch name (e.g., 'PROJ-12345').",
					},
					"isolate": map[string]any{
						"type":        "boolean",
						"default":     true,
						"description": "Create isolated local clone (ALWAYS true for code-writing jobs).",
					},
					"suffix": map[string]any{
						"type":        "string",
						"description": "Optional suffix for parallel tasks on same repo (e.g., 'task1').",
					},
					"base": map[string]any{
						"type":        "string",
						"default":     "main",
						"description": "Base branch to create from (branch created from origin/<base>).",
					},
				},
				"required": []string{"repo", "ticket"},
			},
		},
		Handler: s.toolWorkspaceSetup,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workspace_cleanup",
			Description: "Remove an isolated workspace directory. Accepts either an explicit path OR a correlation_id (workflow ID) — when correlation_id is given, the workspace path is resolved from the workflow's WorkspaceReady event. Safety: only deletes paths under $RICK_REPOS_PATH matching the *-rick-ws-* pattern.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute path to the isolated workspace. Mutually exclusive with correlation_id.",
					},
					"correlation_id": map[string]any{
						"type":        "string",
						"description": "Workflow correlation ID. Resolves the workspace path from the WorkspaceReady event for that workflow. Mutually exclusive with path.",
					},
				},
			},
		},
		Handler: s.toolWorkspaceCleanup,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_workspace_list",
			Description: "List all isolated workspaces under $RICK_REPOS_PATH. Shows git branch and working tree status for each.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		Handler: s.toolWorkspaceList,
	})
}

// --- Handlers ---

type workspaceSetupArgs struct {
	Repo    string `json:"repo"`
	Ticket  string `json:"ticket"`
	Isolate *bool  `json:"isolate"`
	Suffix  string `json:"suffix"`
	Base    string `json:"base"`
}

type workspaceSetupResult struct {
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Base     string `json:"base"`
	HeadSHA  string `json:"head_sha,omitempty"`
	Isolated bool   `json:"isolated"`
}

func (s *Server) toolWorkspaceSetup(_ context.Context, raw json.RawMessage) (any, error) {
	var args workspaceSetupArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if args.Ticket == "" {
		return nil, fmt.Errorf("ticket is required")
	}

	isolate := true
	if args.Isolate != nil {
		isolate = *args.Isolate
	}

	base := args.Base
	if base == "" {
		base = "main"
	}

	result, err := workspace.SetupWorkspace(args.Repo, args.Ticket, "", base, args.Suffix, isolate)
	if err != nil {
		return nil, fmt.Errorf("workspace setup: %w", err)
	}

	headSHA := ""
	if out, gitErr := exec.Command("git", "-C", result.Path, "rev-parse", "HEAD").Output(); gitErr == nil {
		headSHA = strings.TrimSpace(string(out))
	}

	return workspaceSetupResult{
		Path:     result.Path,
		Branch:   result.Branch,
		Base:     result.Base,
		HeadSHA:  headSHA,
		Isolated: result.Isolated,
	}, nil
}

type workspaceCleanupArgs struct {
	Path          string `json:"path"`
	CorrelationID string `json:"correlation_id"`
}

func (s *Server) toolWorkspaceCleanup(ctx context.Context, raw json.RawMessage) (any, error) {
	var args workspaceCleanupArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Path == "" && args.CorrelationID == "" {
		return nil, fmt.Errorf("either path or correlation_id is required")
	}
	if args.Path != "" && args.CorrelationID != "" {
		return nil, fmt.Errorf("path and correlation_id are mutually exclusive")
	}

	path := args.Path
	if args.CorrelationID != "" {
		resolved, err := s.resolveWorkspacePathFromCorrelation(ctx, args.CorrelationID)
		if err != nil {
			return nil, err
		}
		path = resolved
	}

	resolvedPath, err := safeWorkspacePath(path)
	if err != nil {
		return nil, err
	}

	if err := os.RemoveAll(resolvedPath); err != nil {
		return nil, fmt.Errorf("remove workspace: %w", err)
	}

	result := map[string]any{
		"path":    resolvedPath,
		"deleted": true,
	}
	if args.CorrelationID != "" {
		result["correlation_id"] = args.CorrelationID
	}
	return result, nil
}

// resolveWorkspacePathFromCorrelation looks up the workspace path emitted by
// the workspace handler for a given workflow correlation ID. Returns the path
// from the most recent WorkspaceReady event in the correlation chain.
func (s *Server) resolveWorkspacePathFromCorrelation(ctx context.Context, correlationID string) (string, error) {
	if s.deps.Store == nil {
		return "", fmt.Errorf("event store not available")
	}
	events, err := s.deps.Store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return "", fmt.Errorf("load correlation %q: %w", correlationID, err)
	}

	path := ""
	for _, env := range events {
		if env.Type != event.WorkspaceReady {
			continue
		}
		var p event.WorkspaceReadyPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			continue
		}
		if p.Path != "" {
			path = p.Path
		}
	}
	if path == "" {
		return "", fmt.Errorf("no workspace found for correlation %q", correlationID)
	}
	return path, nil
}

type workspaceInfo struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Branch string `json:"branch,omitempty"`
	Dirty  bool   `json:"dirty"`
}

type workspaceListResult struct {
	Workspaces []workspaceInfo `json:"workspaces"`
	Count      int             `json:"count"`
}

func (s *Server) toolWorkspaceList(_ context.Context, _ json.RawMessage) (any, error) {
	reposPath := os.Getenv("RICK_REPOS_PATH")
	if reposPath == "" {
		return nil, fmt.Errorf("RICK_REPOS_PATH environment variable is not set")
	}

	entries, err := os.ReadDir(reposPath)
	if err != nil {
		return nil, fmt.Errorf("read RICK_REPOS_PATH: %w", err)
	}

	var workspaces []workspaceInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.Contains(name, "-rick-ws-") {
			continue
		}

		wsPath := filepath.Join(reposPath, name)
		info := workspaceInfo{
			Path: wsPath,
			Name: name,
		}

		if out, gitErr := exec.Command("git", "-C", wsPath, "branch", "--show-current").Output(); gitErr == nil {
			info.Branch = strings.TrimSpace(string(out))
		}

		if out, gitErr := exec.Command("git", "-C", wsPath, "status", "--porcelain").Output(); gitErr == nil {
			info.Dirty = len(strings.TrimSpace(string(out))) > 0
		}

		workspaces = append(workspaces, info)
	}

	if workspaces == nil {
		workspaces = []workspaceInfo{}
	}

	return workspaceListResult{
		Workspaces: workspaces,
		Count:      len(workspaces),
	}, nil
}

// safeWorkspacePath validates and resolves a workspace path, guarding against
// path traversal via symlinks. Returns the resolved absolute path or an error.
func safeWorkspacePath(path string) (string, error) {
	reposPath := os.Getenv("RICK_REPOS_PATH")
	if reposPath == "" {
		return "", fmt.Errorf("RICK_REPOS_PATH environment variable is not set")
	}

	// Resolve symlinks on both paths to prevent traversal.
	resolvedRepos, err := filepath.EvalSymlinks(reposPath)
	if err != nil {
		return "", fmt.Errorf("resolve RICK_REPOS_PATH: %w", err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	// EvalSymlinks fails if path doesn't exist yet, fall back to Abs.
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		resolved = absPath
	}

	if !strings.HasPrefix(resolved, resolvedRepos+string(filepath.Separator)) && resolved != resolvedRepos {
		return "", fmt.Errorf("refusing to delete path outside $RICK_REPOS_PATH: %s", resolved)
	}

	base := filepath.Base(resolved)
	if !strings.Contains(base, "-rick-ws-") {
		return "", fmt.Errorf("refusing to delete path not matching *-rick-ws-* pattern: %s", base)
	}

	return resolved, nil
}
