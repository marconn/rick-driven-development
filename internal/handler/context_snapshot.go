package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// Size budgets for context snapshots.
const (
	maxTreeEntries   = 500
	maxFileBytes     = 50 << 10 // 50KB total for key file contents
	maxSchemaBytes   = 20 << 10 // 20KB total for schema files
	maxSingleFile    = 10 << 10 // 10KB per individual file
	maxRecentCommits = 10
)

// Directories to always skip when walking the file tree.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"__pycache__": true, ".workflow": true, ".claude": true,
	"dist": true, "build": true, ".next": true, ".nuxt": true,
	"target": true, "coverage": true, ".idea": true, ".vscode": true,
}

// ContextSnapshotHandler captures ground-truth codebase state as events.
// Non-AI handler — runs filesystem and git commands, no LLM calls.
type ContextSnapshotHandler struct {
	store eventstore.Store
	name  string
}

// NewContextSnapshot creates a context snapshot handler with canonical name.
func NewContextSnapshot(d Deps) *ContextSnapshotHandler {
	return &ContextSnapshotHandler{
		store: d.Store,
		name:  "context-snapshot",
	}
}

func (h *ContextSnapshotHandler) Name() string             { return h.name }
func (h *ContextSnapshotHandler) Subscribes() []event.Type { return nil }

func (h *ContextSnapshotHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	workDir, task, err := h.loadContext(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("context-snapshot: load context: %w", err)
	}
	if workDir == "" {
		// No workspace ready — emit empty codebase event so downstream doesn't block.
		return nil, nil
	}

	var events []event.Envelope

	// 1. Codebase snapshot
	codebase := h.snapshotCodebase(workDir, task)
	events = append(events, event.New(event.ContextCodebase, 1, event.MustMarshal(codebase)).
		WithSource("handler:context-snapshot"))

	// 2. Schema snapshot (only if schemas found)
	schema := h.snapshotSchema(workDir)
	if len(schema.Proto) > 0 || len(schema.SQL) > 0 || len(schema.GraphQL) > 0 {
		events = append(events, event.New(event.ContextSchema, 1, event.MustMarshal(schema)).
			WithSource("handler:context-snapshot"))
	}

	// 3. Git snapshot
	gitCtx := h.snapshotGit(workDir)
	if gitCtx.HEAD != "" {
		events = append(events, event.New(event.ContextGit, 1, event.MustMarshal(gitCtx)).
			WithSource("handler:context-snapshot"))
	}

	return events, nil
}

// loadContext reads WorkspaceReady and WorkflowRequested from the correlation chain.
func (h *ContextSnapshotHandler) loadContext(ctx context.Context, correlationID string) (workDir, task string, err error) {
	if correlationID == "" {
		return "", "", nil
	}
	events, err := h.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return "", "", err
	}
	for _, e := range events {
		switch e.Type {
		case event.WorkspaceReady:
			var p event.WorkspaceReadyPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				workDir = p.Path
			}
		case event.WorkflowRequested:
			var p event.WorkflowRequestedPayload
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				task = p.Prompt
			}
		}
	}
	return workDir, task, nil
}

// snapshotCodebase walks the file tree and reads key files.
func (h *ContextSnapshotHandler) snapshotCodebase(workDir, task string) event.ContextCodebasePayload {
	payload := event.ContextCodebasePayload{
		Language:  detectLanguage(workDir),
		Framework: detectFramework(workDir),
	}

	taskLower := strings.ToLower(task)

	var totalFileBytes int
	_ = filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors
		}
		rel, _ := filepath.Rel(workDir, path)
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			if skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}

		if len(payload.Tree) >= maxTreeEntries {
			return fs.SkipAll
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		entry := event.FileEntry{
			Path:     rel,
			Size:     info.Size(),
			Language: langFromExt(filepath.Ext(rel)),
		}
		payload.Tree = append(payload.Tree, entry)

		// Read key files into Files slice (budget-limited).
		if totalFileBytes < maxFileBytes && info.Size() <= maxSingleFile && isKeyFile(rel, taskLower) {
			if content, err := os.ReadFile(path); err == nil {
				payload.Files = append(payload.Files, event.FileSnap{
					Path:    rel,
					Content: string(content),
				})
				totalFileBytes += len(content)
			}
		}

		return nil
	})

	return payload
}

// snapshotSchema finds and reads schema definition files.
func (h *ContextSnapshotHandler) snapshotSchema(workDir string) event.ContextSchemaPayload {
	var payload event.ContextSchemaPayload
	var totalBytes int

	_ = filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if totalBytes >= maxSchemaBytes {
			return fs.SkipAll
		}

		rel, _ := filepath.Rel(workDir, path)
		info, _ := d.Info()
		if info == nil || info.Size() > maxSingleFile {
			return nil
		}

		ext := filepath.Ext(rel)
		var target *[]event.FileSnap

		switch {
		case ext == ".proto":
			target = &payload.Proto
		case ext == ".sql" || strings.Contains(rel, "migration"):
			target = &payload.SQL
		case ext == ".graphql" || ext == ".gql":
			target = &payload.GraphQL
		default:
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		*target = append(*target, event.FileSnap{Path: rel, Content: string(content)})
		totalBytes += len(content)
		return nil
	})

	return payload
}

// snapshotGit captures git state. Best-effort — errors are swallowed.
func (h *ContextSnapshotHandler) snapshotGit(workDir string) event.ContextGitPayload {
	var payload event.ContextGitPayload

	payload.HEAD = gitCmd(workDir, "rev-parse", "--short", "HEAD")
	payload.Branch = gitCmd(workDir, "rev-parse", "--abbrev-ref", "HEAD")

	if log := gitCmd(workDir, "log", "--oneline", fmt.Sprintf("-%d", maxRecentCommits)); log != "" {
		payload.RecentLog = strings.Split(strings.TrimSpace(log), "\n")
	}

	// Diff stat from merge-base (what changed on this branch).
	mergeBase := gitCmd(workDir, "merge-base", "HEAD", "origin/main")
	if mergeBase != "" {
		payload.DiffStat = gitCmd(workDir, "diff", "--stat", mergeBase)
		if files := gitCmd(workDir, "diff", "--name-only", mergeBase); files != "" {
			payload.ModifiedFiles = strings.Split(strings.TrimSpace(files), "\n")
		}
	}

	return payload
}

// gitCmd runs a git command and returns trimmed stdout. Empty on error.
func gitCmd(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// detectLanguage checks for language indicators.
func detectLanguage(dir string) string {
	checks := []struct {
		file string
		lang string
	}{
		{"go.mod", "go"},
		{"package.json", "typescript"},
		{"Cargo.toml", "rust"},
		{"pyproject.toml", "python"},
		{"requirements.txt", "python"},
		{"pom.xml", "java"},
		{"build.gradle", "java"},
		{"Gemfile", "ruby"},
	}
	for _, c := range checks {
		if _, err := os.Stat(filepath.Join(dir, c.file)); err == nil {
			return c.lang
		}
	}
	return ""
}

// detectFramework checks for framework indicators.
func detectFramework(dir string) string {
	// Go frameworks
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			content := string(data)
			switch {
			case strings.Contains(content, "google.golang.org/grpc"):
				return "go-grpc"
			case strings.Contains(content, "github.com/gin-gonic/gin"):
				return "go-gin"
			case strings.Contains(content, "github.com/labstack/echo"):
				return "go-echo"
			}
		}
		return "go"
	}
	// JS/TS frameworks
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		content := string(data)
		switch {
		case strings.Contains(content, "\"vue\""):
			if _, err := os.Stat(filepath.Join(dir, "vite.config")); err == nil {
				return "vue-vite"
			}
			return "vue-webpack"
		case strings.Contains(content, "\"react\""):
			return "react"
		case strings.Contains(content, "\"next\""):
			return "nextjs"
		case strings.Contains(content, "\"nuxt\""):
			return "nuxtjs"
		}
	}
	return ""
}

// langFromExt maps file extensions to language names.
func langFromExt(ext string) string {
	switch ext {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".proto":
		return "protobuf"
	case ".sql":
		return "sql"
	case ".vue":
		return "vue"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	default:
		return ""
	}
}

// isKeyFile returns true if the file should be included in the snapshot.
func isKeyFile(rel, taskLower string) bool {
	base := filepath.Base(rel)

	switch base {
	case "go.mod", "go.sum", "package.json", "Cargo.toml", "pyproject.toml",
		"Makefile", "Dockerfile", "docker-compose.yml",
		"README.md", "CLAUDE.md":
		return true
	}

	if base == "main.go" || base == "index.ts" || base == "index.js" || base == "app.go" {
		return true
	}

	if taskLower != "" {
		relLower := strings.ToLower(rel)
		for _, word := range strings.Fields(taskLower) {
			if len(word) > 3 && strings.Contains(relLower, word) {
				return true
			}
		}
	}

	return false
}
