package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Gemini shells out to the Gemini CLI binary.
// Since the gemini CLI has no --system-prompt flag, the system prompt
// is prepended to the user prompt wrapped in XML tags.
type Gemini struct {
	binaryPath string
}

// NewGemini creates a Gemini backend. binaryPath is the path to the `gemini` CLI binary.
func NewGemini(binaryPath string) *Gemini {
	return &Gemini{binaryPath: binaryPath}
}

func (g *Gemini) Name() string { return "gemini" }

func (g *Gemini) combinePrompt(systemPrompt, userPrompt string) string {
	return fmt.Sprintf("<system_instructions>\n%s\n</system_instructions>\n\n%s", systemPrompt, userPrompt)
}

// buildArgs returns CLI arguments and, when the prompt exceeds maxArgSize,
// the prompt content to pipe via stdin (avoiding OS ARG_MAX limits).
func (g *Gemini) buildArgs(req Request) (args []string, stdinPrompt string) {
	// When resuming with a prompt, skip embedding the system prompt again —
	// the original session already has it.
	var combined string
	if req.SessionID != "" {
		combined = req.UserPrompt
	} else {
		combined = g.combinePrompt(req.SystemPrompt, req.UserPrompt)
	}

	if len(combined) > maxArgSize {
		stdinPrompt = combined
	} else {
		args = append(args, "-p", combined)
	}

	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}

	// Always use stream-json for structured parsing.
	args = append(args, "--output-format", "stream-json")

	if req.Yolo {
		args = append(args, "--yolo")
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}

	return args, stdinPrompt
}

func (g *Gemini) Run(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	args, stdinPrompt := g.buildArgs(req)

	cmd := exec.CommandContext(ctx, g.binaryPath, args...)
	cmd.Dir = req.WorkDir

	var captured bytes.Buffer
	var inner io.Writer = &captured
	if req.Output != nil {
		inner = io.MultiWriter(&captured, req.Output)
	}

	sw := NewStreamWriter(inner, ExtractGeminiText, WithResultCheck(GeminiCheckResult))
	cmd.Stdout = sw

	if stdinPrompt != "" {
		cmd.Stdin = strings.NewReader(stdinPrompt)
	}

	if err := cmd.Run(); err != nil {
		_ = sw.Close()
		return nil, fmt.Errorf("gemini: %w", err)
	}
	_ = sw.Close()

	return &Response{
		Output:     captured.String(),
		StopReason: sw.StopReason(),
		Duration:   time.Since(start),
	}, nil
}
