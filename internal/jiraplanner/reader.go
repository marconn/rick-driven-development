package jiraplanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// PageReaderHandler reads a Confluence page and stores its content in shared
// state for downstream handlers (project-manager, jira-task-creator).
type PageReaderHandler struct {
	confluence *confluence.Client
	store      eventstore.Store
	state      *PlanningState
	logger     *slog.Logger
}

// NewPageReader creates a page-reader handler.
func NewPageReader(conf *confluence.Client, store eventstore.Store, state *PlanningState, logger *slog.Logger) *PageReaderHandler {
	return &PageReaderHandler{confluence: conf, store: store, state: state, logger: logger}
}

func (r *PageReaderHandler) Name() string            { return "page-reader" }
func (r *PageReaderHandler) Subscribes() []event.Type { return nil }

func (r *PageReaderHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	pageID, err := r.extractPageID(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("page-reader: %w", err)
	}
	if r.confluence == nil {
		return nil, fmt.Errorf("page-reader: CONFLUENCE_URL not configured")
	}

	r.logger.Info("reading Confluence page", slog.String("page_id", pageID))

	page, err := r.confluence.ReadPage(ctx, pageID)
	if err != nil {
		return nil, fmt.Errorf("page-reader: read page %s: %w", pageID, err)
	}

	wd := r.state.Get(env.CorrelationID)
	wd.mu.Lock()
	wd.PageID = page.ID
	wd.PageTitle = page.Title
	wd.PageContent = confluence.ExtractTextContent(page.Body)
	wd.mu.Unlock()

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "page-reader",
		Kind:    "confluence-page",
		Summary: fmt.Sprintf("Página %s: %s", page.ID, page.Title),
	})).WithSource("handler:page-reader")

	return []event.Envelope{enrichEvt}, nil
}

// extractPageID finds the Confluence page ID from the WorkflowRequested event.
// Supports "confluence:<id>" source format or a bare numeric ID.
func (r *PageReaderHandler) extractPageID(ctx context.Context, env event.Envelope) (string, error) {
	events, err := r.store.LoadByCorrelation(ctx, env.CorrelationID)
	if err != nil {
		return "", fmt.Errorf("load correlation: %w", err)
	}
	for _, e := range events {
		if e.Type != event.WorkflowRequested {
			continue
		}
		var p event.WorkflowRequestedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if after, ok := strings.CutPrefix(p.Source, "confluence:"); ok {
			return after, nil
		}
		if isNumeric(p.Source) {
			return p.Source, nil
		}
	}
	return "", fmt.Errorf("no Confluence page ID found in workflow params")
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}
