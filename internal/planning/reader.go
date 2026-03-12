package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/confluence"
	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// ReaderHandler is the confluence-reader handler.
// It reads a BTU page from Confluence, parses its sections, and emits
// context enrichment with the structured BTU data.
type ReaderHandler struct {
	confluence *confluence.Client
	store      eventstore.Store
	state      *PlanningState
	logger     *slog.Logger
}

// NewReader creates a confluence-reader handler.
func NewReader(cf *confluence.Client, store eventstore.Store, state *PlanningState, logger *slog.Logger) *ReaderHandler {
	return &ReaderHandler{confluence: cf, store: store, state: state, logger: logger}
}

func (r *ReaderHandler) Name() string            { return "confluence-reader" }
func (r *ReaderHandler) Subscribes() []event.Type { return nil }

func (r *ReaderHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	pageID, err := r.extractPageID(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("confluence-reader: %w", err)
	}

	if r.confluence == nil {
		return nil, fmt.Errorf("confluence-reader: CONFLUENCE_URL not configured")
	}

	r.logger.Info("reading BTU page", slog.String("page_id", pageID))

	page, err := r.confluence.ReadPage(ctx, pageID)
	if err != nil {
		return nil, fmt.Errorf("confluence-reader: read page %s: %w", pageID, err)
	}

	btu := r.parseBTU(page)

	wp := r.state.Get(env.CorrelationID)
	wp.mu.Lock()
	wp.PageID = page.ID
	wp.BTUTitle = page.Title
	wp.BTUContent = btu.content
	wp.BTURawHTML = page.Body
	wp.UserTypes = btu.userTypes
	wp.Devices = btu.devices
	wp.Microservices = btu.microservices
	wp.mu.Unlock()

	r.logger.Info("BTU parsed",
		slog.String("title", page.Title),
		slog.Int("microservices", len(btu.microservices)),
		slog.String("user_types", btu.userTypes),
	)

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "confluence-reader",
		Kind:    "btu",
		Summary: fmt.Sprintf("BTU %s: %s (%d microservicios detectados)", page.ID, page.Title, len(btu.microservices)),
	})).WithSource("handler:confluence-reader")

	return []event.Envelope{enrichEvt}, nil
}

type parsedBTU struct {
	content       string   // full text content
	userTypes     string
	devices       string
	microservices []string // detected microservice names
}

func (r *ReaderHandler) parseBTU(page *confluence.Page) parsedBTU {
	btu := parsedBTU{}

	body := page.Body

	// Extract sections by known BTU headings
	sections := map[string]string{
		"que es":              "",
		"como funciona":       "",
		"tipos de usuario":    "",
		"dispositivo":         "",
		"hipotesis a validar": "",
	}

	lower := strings.ToLower(body)
	for key := range sections {
		idx := strings.Index(lower, key)
		if idx == -1 {
			continue
		}
		rest := body[idx+len(key):]
		nextHeading := findNextHeading(rest)
		if nextHeading > 0 {
			sections[key] = confluence.ExtractTextContent(rest[:nextHeading])
		} else {
			sections[key] = confluence.ExtractTextContent(rest)
		}
	}

	// Build full content
	var sb strings.Builder
	if v := sections["que es"]; v != "" {
		fmt.Fprintf(&sb, "## QUE ES\n%s\n\n", v)
	}
	if v := sections["como funciona"]; v != "" {
		fmt.Fprintf(&sb, "## COMO FUNCIONA\n%s\n\n", v)
	}
	btu.content = sb.String()
	if btu.content == "" {
		btu.content = confluence.ExtractTextContent(body)
	}

	btu.userTypes = sections["tipos de usuario"]
	btu.devices = sections["dispositivo"]

	btu.microservices = detectMicroservices(body)

	return btu
}

// extractPageID gets the Confluence page ID from the workflow requested event.
func (r *ReaderHandler) extractPageID(ctx context.Context, env event.Envelope) (string, error) {
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
		if strings.HasPrefix(p.Source, "confluence:") {
			return strings.TrimPrefix(p.Source, "confluence:"), nil
		}
		if pageID := extractConfluencePageID(p.Source); pageID != "" {
			return pageID, nil
		}
		if pageID := extractConfluencePageID(p.Prompt); pageID != "" {
			return pageID, nil
		}
		if isNumeric(p.Source) {
			return p.Source, nil
		}
	}
	return "", fmt.Errorf("no Confluence page ID found in workflow params")
}

// findNextHeading returns the position of the next HTML heading tag.
func findNextHeading(html string) int {
	lower := strings.ToLower(html)
	minPos := -1
	for _, tag := range []string{"<h1", "<h2", "<h3", "<h4"} {
		pos := strings.Index(lower, tag)
		if pos > 0 && (minPos == -1 || pos < minPos) {
			minPos = pos
		}
	}
	return minPos
}

// detectMicroservices finds microservice name patterns in content.
// Looks for [microservice-name] patterns and known service names.
var bracketPattern = regexp.MustCompile(`\[([a-z][a-z0-9-]+(?:-(?:web|api|service))?)\]`)

// commonSpanishWords are bracket-enclosed words that aren't microservice names.
var commonSpanishWords = map[string]bool{
	"nombre": true, "ejemplo": true, "nota": true, "ver": true,
	"tipo": true, "opcional": true, "requerido": true, "campo": true,
	"dato": true, "valor": true, "estado": true, "nuevo": true,
	"microservicio": true, "servicio": true, "tarea": true,
}

func detectMicroservices(html string) []string {
	seen := make(map[string]bool)
	var result []string

	matches := bracketPattern.FindAllStringSubmatch(html, -1)
	for _, m := range matches {
		name := m[1]
		if !seen[name] && !commonSpanishWords[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	return result
}

var confluenceURLRegex = regexp.MustCompile(`/pages/(\d+)`)

func extractConfluencePageID(text string) string {
	matches := confluenceURLRegex.FindStringSubmatch(text)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}
