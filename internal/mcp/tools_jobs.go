package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/persona"
)

func (s *Server) registerJobTools() {

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_consult",
			Description: "Spawn an AI persona for a one-shot advisory question. No workflow, no events, no aggregate. Returns a job ID for async polling via rick_job_status/rick_job_output. Use this for quick questions: 'Ask an architect about X', 'Get a QA review of this plan'.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "The prompt to send to the AI persona.",
					},
					"mode": map[string]any{
						"type":        "string",
						"enum":        []string{"architect", "reviewer", "qa", "researcher", "developer"},
						"default":     "architect",
						"description": "Which persona to use.",
					},
					"backend": map[string]any{
						"type":        "string",
						"enum":        []string{"claude", "gemini"},
						"description": "AI backend. Defaults to server's configured backend.",
					},
					"model": map[string]any{
						"type":        "string",
						"description": "Override model name.",
					},
					"context_files": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "File or directory paths to attach as context.",
					},
					"work_dir": map[string]any{
						"type":        "string",
						"description": "Working directory for backend execution.",
					},
					"yolo": map[string]any{
						"type":        "boolean",
						"default":     true,
						"description": "Skip AI backend permission checks.",
					},
				},
				"required": []string{"prompt"},
			},
		},
		Handler: s.toolConsult,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_run",
			Description: "Spawn an AI backend with tools enabled for direct execution. For implementation, debugging, refactoring. No workflow, no events. Returns a job ID for async polling. The backend has full tool access (file editing, terminal, etc).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{
						"type":        "string",
						"description": "The task prompt for the AI backend.",
					},
					"mode": map[string]any{
						"type":        "string",
						"enum":        []string{"developer", "architect", "researcher"},
						"default":     "developer",
						"description": "Which persona mode to use.",
					},
					"backend": map[string]any{
						"type":        "string",
						"enum":        []string{"claude", "gemini"},
						"description": "AI backend. Defaults to server's configured backend.",
					},
					"model": map[string]any{
						"type":        "string",
						"description": "Override model name.",
					},
					"context_files": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "File or directory paths to attach as context.",
					},
					"work_dir": map[string]any{
						"type":        "string",
						"description": "Working directory for backend execution.",
					},
					"yolo": map[string]any{
						"type":        "boolean",
						"default":     true,
						"description": "Skip AI backend permission checks.",
					},
					"mcp_config": map[string]any{
						"type":        "object",
						"description": "MCP server configs to give the spawned AI access to external tools (Claude only).",
					},
				},
				"required": []string{"prompt"},
			},
		},
		Handler: s.toolRun,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_job_status",
			Description: "Get the status of an async job spawned by rick_consult or rick_run.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "The job ID returned by rick_consult or rick_run.",
					},
				},
				"required": []string{"job_id"},
			},
		},
		Handler: s.toolJobStatus,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_job_output",
			Description: "Get the output of a completed async job. Supports incremental reads via offset for streaming large outputs.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "The job ID.",
					},
					"offset": map[string]any{
						"type":        "integer",
						"default":     0,
						"description": "Character offset to start reading from (for incremental reads).",
					},
					"max_length": map[string]any{
						"type":        "integer",
						"default":     50000,
						"description": "Maximum characters to return.",
					},
				},
				"required": []string{"job_id"},
			},
		},
		Handler: s.toolJobOutput,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_job_cancel",
			Description: "Cancel a running async job.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "The job ID to cancel.",
					},
				},
				"required": []string{"job_id"},
			},
		},
		Handler: s.toolJobCancel,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jobs",
			Description: "List all tracked async jobs with their status, type, and start time.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		Handler: s.toolJobsList,
	})
}

// --- Tool Handlers ---

type consultArgs struct {
	Prompt       string   `json:"prompt"`
	Mode         string   `json:"mode"`
	Backend      string   `json:"backend"`
	Model        string   `json:"model"`
	ContextFiles []string `json:"context_files"`
	WorkDir      string   `json:"work_dir"`
	Yolo         *bool    `json:"yolo"`
}

type consultResult struct {
	JobID   string `json:"job_id"`
	Status  string `json:"status"`
	Mode    string `json:"mode"`
	Backend string `json:"backend"`
}

func (s *Server) toolConsult(_ context.Context, raw json.RawMessage) (any, error) {
	var args consultArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if args.Mode == "" {
		args.Mode = "architect"
	}

	be, err := s.resolveBackend(args.Backend)
	if err != nil {
		return nil, err
	}

	systemPrompt, err := s.loadPersonaPrompt(args.Mode)
	if err != nil {
		return nil, err
	}

	userPrompt := args.Prompt
	if len(args.ContextFiles) > 0 {
		userPrompt = fmt.Sprintf("%s\n\nContext files: %v", args.Prompt, args.ContextFiles)
	}

	yolo := s.deps.Yolo
	if args.Yolo != nil {
		yolo = *args.Yolo
	}

	req := backend.Request{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		Model:        args.Model,
		WorkDir:      s.resolveWorkDir(args.WorkDir),
		Yolo:         yolo,
	}

	jobID := s.jobs.Launch(be, req, "consult", args.Mode)

	return consultResult{
		JobID:   jobID,
		Status:  "running",
		Mode:    args.Mode,
		Backend: be.Name(),
	}, nil
}

type runArgs struct {
	Prompt       string          `json:"prompt"`
	Mode         string          `json:"mode"`
	Backend      string          `json:"backend"`
	Model        string          `json:"model"`
	ContextFiles []string        `json:"context_files"`
	WorkDir      string          `json:"work_dir"`
	Yolo         *bool           `json:"yolo"`
	MCPConfig    json.RawMessage `json:"mcp_config"`
}

func (s *Server) toolRun(_ context.Context, raw json.RawMessage) (any, error) {
	var args runArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if args.Mode == "" {
		args.Mode = "developer"
	}

	be, err := s.resolveBackend(args.Backend)
	if err != nil {
		return nil, err
	}

	systemPrompt, err := s.loadPersonaPrompt(args.Mode)
	if err != nil {
		return nil, err
	}

	userPrompt := args.Prompt
	if len(args.ContextFiles) > 0 {
		userPrompt = fmt.Sprintf("%s\n\nContext files: %v", args.Prompt, args.ContextFiles)
	}

	yolo := s.deps.Yolo
	if args.Yolo != nil {
		yolo = *args.Yolo
	}

	req := backend.Request{
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		Model:        args.Model,
		WorkDir:      s.resolveWorkDir(args.WorkDir),
		Yolo:         yolo,
	}
	if len(args.MCPConfig) > 0 {
		req.MCPConfig = string(args.MCPConfig)
	}

	jobID := s.jobs.Launch(be, req, "run", args.Mode)

	return consultResult{
		JobID:   jobID,
		Status:  "running",
		Mode:    args.Mode,
		Backend: be.Name(),
	}, nil
}

// --- Job Management Handlers ---

type jobIDArgs struct {
	JobID string `json:"job_id"`
}

type jobStatusResult struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Type          string `json:"type"`
	StartedAt     string `json:"started_at"`
	Duration      string `json:"duration,omitempty"`
	OutputSize    int    `json:"output_size"`
	Error         string `json:"error,omitempty"`
	PromptPreview string `json:"prompt_preview"`
}

func (s *Server) toolJobStatus(_ context.Context, raw json.RawMessage) (any, error) {
	var args jobIDArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.JobID == "" {
		return nil, fmt.Errorf("job_id is required")
	}

	job, err := s.jobs.Get(args.JobID)
	if err != nil {
		return nil, err
	}

	return jobStatusResult{
		ID:            job.ID,
		Status:        string(job.Status),
		Type:          job.Type,
		StartedAt:     job.StartedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Duration:      job.Duration,
		OutputSize:    job.OutputSize,
		Error:         job.Error,
		PromptPreview: job.PromptPreview,
	}, nil
}

type jobOutputArgs struct {
	JobID     string `json:"job_id"`
	Offset    int    `json:"offset"`
	MaxLength int    `json:"max_length"`
}

type jobOutputResult struct {
	Content   string `json:"content"`
	TotalSize int    `json:"total_size"`
	Truncated bool   `json:"truncated"`
}

func (s *Server) toolJobOutput(_ context.Context, raw json.RawMessage) (any, error) {
	var args jobOutputArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.JobID == "" {
		return nil, fmt.Errorf("job_id is required")
	}
	if args.MaxLength <= 0 {
		args.MaxLength = 50000
	}

	job, err := s.jobs.Get(args.JobID)
	if err != nil {
		return nil, err
	}

	output := job.Output
	total := len(output)

	if args.Offset > 0 && args.Offset < total {
		output = output[args.Offset:]
	} else if args.Offset >= total {
		output = ""
	}

	truncated := false
	if len(output) > args.MaxLength {
		output = output[:args.MaxLength]
		truncated = true
	}

	return jobOutputResult{
		Content:   output,
		TotalSize: total,
		Truncated: truncated,
	}, nil
}

func (s *Server) toolJobCancel(_ context.Context, raw json.RawMessage) (any, error) {
	var args jobIDArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.JobID == "" {
		return nil, fmt.Errorf("job_id is required")
	}

	if err := s.jobs.Cancel(args.JobID); err != nil {
		return nil, err
	}

	return map[string]any{"job_id": args.JobID, "cancelled": true}, nil
}

type jobsListResult struct {
	Jobs  []Job `json:"jobs"`
	Count int   `json:"count"`
}

func (s *Server) toolJobsList(_ context.Context, _ json.RawMessage) (any, error) {
	jobs := s.jobs.List()
	return jobsListResult{Jobs: jobs, Count: len(jobs)}, nil
}

// --- Helpers ---

func (s *Server) resolveBackend(name string) (backend.Backend, error) {
	if name == "" {
		if s.deps.Backend != nil {
			return s.deps.Backend, nil
		}
		name = s.deps.BackendName
	}
	if name == "" {
		name = "claude"
	}
	return backend.New(name)
}

func (s *Server) resolveWorkDir(dir string) string {
	if dir != "" {
		return dir
	}
	return s.deps.WorkDir
}

func (s *Server) loadPersonaPrompt(mode string) (string, error) {
	reg := persona.DefaultRegistry()
	prompt, err := reg.LoadSystemPrompt(mode)
	if err != nil {
		return "", fmt.Errorf("load persona prompt for %q: %w", mode, err)
	}
	return prompt, nil
}
