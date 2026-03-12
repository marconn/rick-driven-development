package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (s *Server) registerConfluenceTools() {

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_confluence_read",
			Description: "Read a Confluence page by ID or URL. Returns title, body content, version, and space key.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"page_id": map[string]any{
						"type":        "string",
						"description": "Confluence page ID (numeric) or full page URL.",
					},
				},
				"required": []string{"page_id"},
			},
		},
		Handler: s.toolConfluenceRead,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_confluence_write",
			Description: "Update a section of a Confluence page. Finds the heading and replaces content under it. Content is provided as markdown and converted to Confluence storage format.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"page_id": map[string]any{
						"type":        "string",
						"description": "Confluence page ID (numeric) or full page URL.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "Content to write (markdown or HTML).",
					},
					"after_heading": map[string]any{
						"type":        "string",
						"description": "Insert after this heading (e.g., 'Plan Tecnico').",
					},
					},
				"required": []string{"page_id", "content", "after_heading"},
			},
		},
		Handler: s.toolConfluenceWrite,
	})
}

// --- Handlers ---

func (s *Server) requireConfluence() error {
	if s.deps.Confluence == nil {
		return fmt.Errorf("Confluence client not configured (set CONFLUENCE_URL, CONFLUENCE_EMAIL, CONFLUENCE_TOKEN)")
	}
	return nil
}

// extractPageID extracts a numeric page ID from either a raw ID or Confluence URL.
func extractPageID(input string) string {
	if idx := strings.LastIndex(input, "/pages/"); idx >= 0 {
		rest := input[idx+7:]
		if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
			return rest[:slashIdx]
		}
		return rest
	}
	// Already a numeric ID or similar.
	return input
}

type confluenceReadArgs struct {
	PageID string `json:"page_id"`
}

func (s *Server) toolConfluenceRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var args confluenceReadArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.PageID == "" {
		return nil, fmt.Errorf("page_id is required")
	}
	if err := s.requireConfluence(); err != nil {
		return nil, err
	}

	pageID := extractPageID(args.PageID)

	page, err := s.deps.Confluence.ReadPage(ctx, pageID)
	if err != nil {
		return nil, fmt.Errorf("read page: %w", err)
	}

	return map[string]any{
		"id":        page.ID,
		"title":     page.Title,
		"body":      page.Body,
		"version":   page.Version,
		"space_key": page.SpaceKey,
	}, nil
}

type confluenceWriteArgs struct {
	PageID       string `json:"page_id"`
	Content      string `json:"content"`
	AfterHeading string `json:"after_heading"`
}

func (s *Server) toolConfluenceWrite(ctx context.Context, raw json.RawMessage) (any, error) {
	var args confluenceWriteArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.PageID == "" || args.Content == "" {
		return nil, fmt.Errorf("page_id and content are required")
	}
	if err := s.requireConfluence(); err != nil {
		return nil, err
	}

	pageID := extractPageID(args.PageID)

	page, err := s.deps.Confluence.ReadPage(ctx, pageID)
	if err != nil {
		return nil, fmt.Errorf("read page: %w", err)
	}

	if args.AfterHeading != "" {
		if err := s.deps.Confluence.UpdatePageSection(ctx, page, args.AfterHeading, args.Content); err != nil {
			return nil, fmt.Errorf("update section: %w", err)
		}
	} else {
		return nil, fmt.Errorf("after_heading is required for section updates (full page overwrites not supported for safety)")
	}

	return map[string]any{
		"page_id":  pageID,
		"title":    page.Title,
		"updated":  true,
		"heading":  args.AfterHeading,
	}, nil
}
