// Package jira provides a thin REST client for the Jira Cloud API v3.
// Reads credentials from JIRA_URL, JIRA_EMAIL, JIRA_TOKEN env vars.
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/adf"
)

// Client wraps an HTTP client configured for Jira basic auth.
type Client struct {
	baseURL    string
	email      string
	token      string
	project    string // default project key (e.g. "PROJ")
	teamID     string // numeric ID for customfield_11533 (Team)
	httpClient *http.Client
}

// NewClientFromEnv creates a Client from JIRA_URL, JIRA_EMAIL, JIRA_TOKEN
// environment variables. Returns nil (not an error) when vars are unset,
// allowing callers to degrade gracefully.
func NewClientFromEnv() *Client {
	baseURL := os.Getenv("JIRA_URL")
	email := os.Getenv("JIRA_EMAIL")
	token := os.Getenv("JIRA_TOKEN")

	if baseURL == "" || email == "" || token == "" {
		return nil
	}

	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		email:      email,
		token:      token,
		project:    envOrDefault("JIRA_PROJECT", "PROJ"),
		teamID:     os.Getenv("JIRA_TEAM_ID"),
		httpClient: &http.Client{},
	}
}

// BaseURL returns the configured Jira instance URL.
func (c *Client) BaseURL() string { return c.baseURL }

// NewClient creates a Client with explicit credentials. Intended for tests.
func NewClient(baseURL, email, token string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		email:      email,
		token:      token,
		httpClient: &http.Client{},
	}
}

// WithProject sets the default project key on the client.
func (c *Client) WithProject(project string) *Client {
	c.project = project
	return c
}

// WithTeamID sets the team custom field ID on the client.
func (c *Client) WithTeamID(teamID string) *Client {
	c.teamID = teamID
	return c
}

// Issue holds the fields we read from the Jira REST API.
type Issue struct {
	ID     string `json:"id"` // internal numeric ID
	Key    string `json:"key"`
	Fields struct {
		Summary     string `json:"summary"`
		Description any    `json:"description"` // ADF object or plain string
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Labels     []string `json:"labels"`
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
		// Acceptance criteria may live in different custom fields.
		AcceptanceCriteria10035 any `json:"customfield_10035"`
		AcceptanceCriteria10036 any `json:"customfield_10036"`
		// Microservice select field — maps to repo directory name under RICK_REPOS_PATH.
		Microservice *struct {
			Value string `json:"value"`
		} `json:"customfield_11538"`
	} `json:"fields"`
}

// ComponentNames returns component name strings from the issue.
func (i *Issue) ComponentNames() []string {
	names := make([]string, 0, len(i.Fields.Components))
	for _, c := range i.Fields.Components {
		names = append(names, c.Name)
	}
	return names
}

// MicroserviceName returns the Microservice select field value, or "".
func (i *Issue) MicroserviceName() string {
	if i.Fields.Microservice != nil {
		return i.Fields.Microservice.Value
	}
	return ""
}

// AddLabel appends a single label to an issue without removing existing ones.
func (c *Client) AddLabel(ctx context.Context, issueKey, label string) error {
	url := c.baseURL + "/rest/api/3/issue/" + issueKey

	body := map[string]any{
		"update": map[string]any{
			"labels": []map[string]string{{"add": label}},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http put %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jira API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return nil
}

// SetMicroservice sets the Microservice select field (customfield_11538).
// If the value is not a valid option, falls back to adding a repo:<name> label.
func (c *Client) SetMicroservice(ctx context.Context, issueKey, name string) (method string, err error) {
	err = c.UpdateField(ctx, issueKey, "customfield_11538", map[string]any{"value": name})
	if err == nil {
		return "microservice_field", nil
	}

	// Microservice option doesn't exist — fall back to label.
	labelErr := c.AddLabel(ctx, issueKey, "repo:"+name)
	if labelErr != nil {
		return "", fmt.Errorf("set microservice field failed (%w) and fallback label also failed: %w", err, labelErr)
	}
	return "label", nil
}

// FetchIssue calls GET /rest/api/3/issue/{key}.
func (c *Client) FetchIssue(ctx context.Context, key string) (*Issue, error) {
	url := c.baseURL + "/rest/api/3/issue/" + key

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, fmt.Errorf("unmarshal jira issue: %w", err)
	}

	return &issue, nil
}

// UpdateField PUTs a single field value on a Jira issue.
func (c *Client) UpdateField(ctx context.Context, issueKey, fieldID string, value any) error {
	url := c.baseURL + "/rest/api/3/issue/" + issueKey

	body := map[string]any{
		"fields": map[string]any{
			fieldID: value,
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http put %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jira API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return nil
}

// CreateEpic creates a Jira Epic and returns its issue key (e.g. "PROJ-123").
func (c *Client) CreateEpic(ctx context.Context, title, description string) (string, error) {
	fields := map[string]any{
		"project":           map[string]any{"key": c.project},
		"issuetype":         map[string]any{"name": "Epic"},
		"summary":           title,
		"description":       MarkdownToADF(description),
		"customfield_10201": title, // Epic Name
	}
	if c.teamID != "" {
		fields["customfield_11533"] = map[string]any{"id": c.teamID}
	}
	return c.createIssue(ctx, fields)
}

// CreateTask creates a Jira Task linked to the given epic and returns its key.
func (c *Client) CreateTask(ctx context.Context, epicKey, title, description string, storyPoints float64) (string, error) {
	fields := map[string]any{
		"project":           map[string]any{"key": c.project},
		"issuetype":         map[string]any{"name": "Task"},
		"summary":           title,
		"description":       MarkdownToADF(description),
		"customfield_10200": epicKey, // Epic Link
	}
	if c.teamID != "" {
		fields["customfield_11533"] = map[string]any{"id": c.teamID}
	}
	if storyPoints > 0 {
		fields["customfield_10004"] = storyPoints
	}
	return c.createIssue(ctx, fields)
}

// CreateOption configures optional fields for CreateIssue.
type CreateOption func(fields map[string]any)

// WithProject overrides the default project key.
func WithProject(key string) CreateOption {
	return func(fields map[string]any) {
		if key != "" {
			fields["project"] = map[string]any{"key": key}
		}
	}
}

// WithEpicLink sets the parent epic for the issue.
func WithEpicLink(epicKey string) CreateOption {
	return func(fields map[string]any) {
		if epicKey != "" {
			fields["customfield_10200"] = epicKey
		}
	}
}

// WithStoryPoints sets story points on the issue.
func WithStoryPoints(points float64) CreateOption {
	return func(fields map[string]any) {
		if points > 0 {
			fields["customfield_10004"] = points
		}
	}
}

// WithLabels sets labels on the issue.
func WithLabels(labels []string) CreateOption {
	return func(fields map[string]any) {
		if len(labels) > 0 {
			fields["labels"] = labels
		}
	}
}

// WithComponents sets components on the issue.
func WithComponents(names []string) CreateOption {
	return func(fields map[string]any) {
		if len(names) > 0 {
			comps := make([]map[string]any, len(names))
			for i, n := range names {
				comps[i] = map[string]any{"name": n}
			}
			fields["components"] = comps
		}
	}
}

// WithPriority sets the issue priority.
func WithPriority(name string) CreateOption {
	return func(fields map[string]any) {
		if name != "" {
			fields["priority"] = map[string]any{"name": name}
		}
	}
}

// WithTeam overrides the default team (customfield_11533) for a single issue.
func WithTeam(teamID string) CreateOption {
	return func(fields map[string]any) {
		if teamID != "" {
			fields["customfield_11533"] = map[string]any{"id": teamID}
		}
	}
}

// CreateIssue creates a Jira issue of any type and returns its key.
func (c *Client) CreateIssue(ctx context.Context, issueType, summary, description string, opts ...CreateOption) (string, error) {
	fields := map[string]any{
		"project":   map[string]any{"key": c.project},
		"issuetype": map[string]any{"name": issueType},
		"summary":   summary,
	}
	if description != "" {
		fields["description"] = MarkdownToADF(description)
	}
	if issueType == "Epic" {
		fields["customfield_10201"] = summary // Epic Name
	}
	if c.teamID != "" {
		fields["customfield_11533"] = map[string]any{"id": c.teamID}
	}
	for _, opt := range opts {
		opt(fields)
	}
	return c.createIssue(ctx, fields)
}

func (c *Client) createIssue(ctx context.Context, fields map[string]any) (string, error) {
	body, err := json.Marshal(map[string]any{"fields": fields})
	if err != nil {
		return "", fmt.Errorf("jira: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/rest/api/3/issue", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("jira: build request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("jira: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("jira: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("jira: decode response: %w", err)
	}
	return result.Key, nil
}

// LinkIssues creates a "Blocks" link: blocker blocks blocked.
func (c *Client) LinkIssues(ctx context.Context, blockerKey, blockedKey string) error {
	body, err := json.Marshal(map[string]any{
		"type":         map[string]any{"name": "Blocks"},
		"inwardIssue":  map[string]any{"key": blockedKey},
		"outwardIssue": map[string]any{"key": blockerKey},
	})
	if err != nil {
		return fmt.Errorf("jira: marshal link: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/rest/api/3/issueLink", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("jira: build link request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jira: link request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jira: link HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// MarkdownToADF converts markdown text to Atlassian Document Format.
// Delegates to internal/adf, which uses goldmark + GFM extensions and supports
// the full set of nodes Jira accepts (h1-h6, paragraphs, lists, blockquotes,
// fenced code blocks, tables, links, italics, code spans, strikethrough, hard
// breaks). Soft line breaks within a paragraph are emitted as ADF hardBreak
// nodes so multi-line markdown keeps its visual line structure in Jira.
func MarkdownToADF(text string) map[string]any {
	return adf.FromMarkdown(text)
}

// RawIssue gives full access to all fields including custom ones.
type RawIssue struct {
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
}

// FetchRawIssue calls GET /rest/api/3/issue/{key} and returns all fields as raw JSON.
func (c *Client) FetchRawIssue(ctx context.Context, key string) (*RawIssue, error) {
	url := c.baseURL + "/rest/api/3/issue/" + key

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var issue RawIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, fmt.Errorf("unmarshal jira issue: %w", err)
	}

	return &issue, nil
}

// SearchResult holds the response from a JQL search.
type SearchResult struct {
	Total  int           `json:"total"`
	Issues []SearchIssue `json:"issues"`
}

// SearchIssue is a Jira issue from the search API.
type SearchIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
	} `json:"fields"`
}

// Search executes a JQL query and returns matching issues.
// Uses POST /rest/api/3/search/jql (Atlassian deprecated GET /rest/api/3/search).
func (c *Client) Search(ctx context.Context, jql string, maxResults int) (*SearchResult, error) {
	reqURL := c.baseURL + "/rest/api/3/search/jql"

	payload, err := json.Marshal(map[string]any{
		"jql":        jql,
		"maxResults": maxResults,
	})
	if err != nil {
		return nil, fmt.Errorf("jira: search: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("jira: search: create request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira: search: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("jira: search: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira: search: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result SearchResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("jira: search: unmarshal: %w", err)
	}
	return &result, nil
}

// GetIssue retrieves a single issue with all fields as raw JSON.
// Alias for FetchRawIssue — provided for API compatibility with jirapoller.
func (c *Client) GetIssue(ctx context.Context, key string) (*RawIssue, error) {
	return c.FetchRawIssue(ctx, key)
}

// AddComment posts a comment to a Jira issue using ADF format.
func (c *Client) AddComment(ctx context.Context, key, body string) error {
	commentBody := map[string]any{
		"body": map[string]any{
			"version": 1,
			"type":    "doc",
			"content": []any{
				map[string]any{
					"type": "paragraph",
					"content": []any{
						map[string]any{
							"type": "text",
							"text": body,
						},
					},
				},
			},
		},
	}
	payload, err := json.Marshal(commentBody)
	if err != nil {
		return fmt.Errorf("jira: marshal comment: %w", err)
	}

	reqURL := c.baseURL + "/rest/api/3/issue/" + key + "/comment"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("jira: add comment: create request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jira: add comment to %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jira: add comment HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// ExtractTextField extracts a text value from a raw issue field.
// For ADF content, extracts plain text. For strings, returns directly.
func ExtractTextField(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var doc struct {
		Content []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err == nil {
		var parts []string
		for _, block := range doc.Content {
			for _, inline := range block.Content {
				if inline.Text != "" {
					parts = append(parts, inline.Text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// ADFToPlainText converts an Atlassian Document Format value or plain
// string to readable text. Returns empty string for nil input.
func ADFToPlainText(v any) string {
	if v == nil {
		return ""
	}

	if s, ok := v.(string); ok {
		return s
	}

	if m, ok := v.(map[string]any); ok {
		return extractADFText(m)
	}

	return ""
}

// extractADFText recursively extracts text content from ADF node maps.
func extractADFText(node map[string]any) string {
	var parts []string

	if text, ok := node["text"].(string); ok && text != "" {
		parts = append(parts, text)
	}

	if content, ok := node["content"].([]any); ok {
		for _, child := range content {
			if childMap, ok := child.(map[string]any); ok {
				if t := extractADFText(childMap); t != "" {
					parts = append(parts, t)
				}
			}
		}
	}

	return strings.Join(parts, " ")
}

// TransitionIssue moves a Jira issue to a new status by name.
// Finds the transition ID by matching the target status name, then executes it.
func (c *Client) TransitionIssue(ctx context.Context, key, targetStatus string) error {
	// Get available transitions.
	reqURL := c.baseURL + "/rest/api/3/issue/" + key + "/transitions"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("jira: build transitions request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jira: get transitions: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jira: transitions HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("jira: decode transitions: %w", err)
	}

	target := strings.ToUpper(targetStatus)
	var transitionID string
	for _, t := range result.Transitions {
		if strings.EqualFold(t.To.Name, targetStatus) || strings.ToUpper(t.To.Name) == target {
			transitionID = t.ID
			break
		}
	}
	if transitionID == "" {
		available := make([]string, 0, len(result.Transitions))
		for _, t := range result.Transitions {
			available = append(available, t.To.Name)
		}
		return fmt.Errorf("jira: no transition to %q found (available: %v)", targetStatus, available)
	}

	// Execute transition.
	payload, _ := json.Marshal(map[string]any{
		"transition": map[string]any{"id": transitionID},
	})
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("jira: build transition request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err = c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jira: execute transition: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jira: transition HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// EpicChildIssue represents a child issue of an epic from a JQL search.
type EpicChildIssue struct {
	Key       string   `json:"key"`
	Summary   string   `json:"summary"`
	Status    string   `json:"status"`
	Assignee  string   `json:"assignee,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Points    float64  `json:"points,omitempty"`
	IssueType string   `json:"issue_type,omitempty"`
}

// FetchEpicChildren returns all child issues of an epic.
func (c *Client) FetchEpicChildren(ctx context.Context, epicKey string, includeClosed bool) ([]EpicChildIssue, error) {
	jql := fmt.Sprintf(`"Epic Link" = %s`, epicKey)
	if !includeClosed {
		jql += ` AND status != Done AND status != Cancelled AND status != Closed`
	}
	jql += " ORDER BY rank ASC"

	reqURL := c.baseURL + "/rest/api/3/search/jql"
	payload, err := json.Marshal(map[string]any{
		"jql":        jql,
		"maxResults": 100,
		"fields":     []string{"summary", "status", "assignee", "labels", "customfield_10004", "issuetype"},
	})
	if err != nil {
		return nil, fmt.Errorf("jira: epic children: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("jira: epic children request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira: epic children: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira: epic children HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Issues []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary   string `json:"summary"`
				Status    struct{ Name string } `json:"status"`
				Assignee  *struct{ DisplayName string } `json:"assignee"`
				Labels    []string `json:"labels"`
				Points    *float64 `json:"customfield_10004"`
				IssueType struct{ Name string } `json:"issuetype"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("jira: decode epic children: %w", err)
	}

	children := make([]EpicChildIssue, 0, len(result.Issues))
	for _, issue := range result.Issues {
		child := EpicChildIssue{
			Key:       issue.Key,
			Summary:   issue.Fields.Summary,
			Status:    issue.Fields.Status.Name,
			Labels:    issue.Fields.Labels,
			IssueType: issue.Fields.IssueType.Name,
		}
		if issue.Fields.Assignee != nil {
			child.Assignee = issue.Fields.Assignee.DisplayName
		}
		if issue.Fields.Points != nil {
			child.Points = *issue.Fields.Points
		}
		children = append(children, child)
	}
	return children, nil
}

// LinkIssuesWithType creates an issue link of the specified type.
func (c *Client) LinkIssuesWithType(ctx context.Context, fromKey, toKey, linkType string) error {
	body, err := json.Marshal(map[string]any{
		"type":         map[string]any{"name": linkType},
		"outwardIssue": map[string]any{"key": fromKey},
		"inwardIssue":  map[string]any{"key": toKey},
	})
	if err != nil {
		return fmt.Errorf("jira: marshal link: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/rest/api/3/issueLink", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("jira: build link request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jira: link request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jira: link HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// FetchIssueLinks returns all issue links for a given issue key.
func (c *Client) FetchIssueLinks(ctx context.Context, key string) ([]IssueLink, error) {
	reqURL := c.baseURL + "/rest/api/3/issue/" + key + "?fields=issuelinks"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("jira: build issue links request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira: fetch issue links: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira: issue links HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Fields struct {
			IssueLinks []struct {
				ID   string `json:"id"`
				Type struct {
					Name    string `json:"name"`
					Inward  string `json:"inward"`
					Outward string `json:"outward"`
				} `json:"type"`
				InwardIssue  *struct{ Key string } `json:"inwardIssue"`
				OutwardIssue *struct{ Key string } `json:"outwardIssue"`
			} `json:"issuelinks"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("jira: decode issue links: %w", err)
	}

	links := make([]IssueLink, 0, len(result.Fields.IssueLinks))
	for _, l := range result.Fields.IssueLinks {
		link := IssueLink{
			ID:   l.ID,
			Type: l.Type.Name,
		}
		if l.OutwardIssue != nil {
			link.OutwardKey = l.OutwardIssue.Key
			link.Direction = "outward"
			link.Label = l.Type.Outward
		}
		if l.InwardIssue != nil {
			link.InwardKey = l.InwardIssue.Key
			link.Direction = "inward"
			link.Label = l.Type.Inward
		}
		links = append(links, link)
	}
	return links, nil
}

// IssueLink represents a link between two Jira issues.
type IssueLink struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Direction  string `json:"direction"` // "inward" or "outward"
	Label      string `json:"label"`     // human-readable like "blocks" or "is blocked by"
	InwardKey  string `json:"inward_key,omitempty"`
	OutwardKey string `json:"outward_key,omitempty"`
}

// DeleteIssueLink removes an issue link by its ID.
func (c *Client) DeleteIssueLink(ctx context.Context, linkID string) error {
	reqURL := c.baseURL + "/rest/api/3/issueLink/" + linkID
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, reqURL, nil)
	if err != nil {
		return fmt.Errorf("jira: build delete link request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("jira: delete issue link: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("jira: delete issue link HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// PRLink represents a pull request linked to a Jira issue via the GitHub integration.
type PRLink struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Status string `json:"status"`
	Source string `json:"source,omitempty"` // repo name
}

// FetchPRLinks returns pull request links for an issue via the Jira dev-status API.
// Requires the GitHub for Jira integration to be installed on the Jira instance.
func (c *Client) FetchPRLinks(ctx context.Context, issueKey string) ([]PRLink, error) {
	// First get the issue's internal numeric ID.
	issue, err := c.FetchIssue(ctx, issueKey)
	if err != nil {
		return nil, fmt.Errorf("fetch issue for PR links: %w", err)
	}
	if issue.ID == "" {
		return nil, fmt.Errorf("issue %s has no internal ID", issueKey)
	}

	reqURL := c.baseURL + "/rest/dev-status/latest/issue/detail?issueId=" + issue.ID +
		"&applicationType=GitHub&dataType=pullrequest"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("jira: build dev-status request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jira: dev-status request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jira: dev-status HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		Detail []struct {
			PullRequests []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				URL    string `json:"url"`
				Status string `json:"status"`
				Source struct {
					URL string `json:"url"`
				} `json:"source"`
			} `json:"pullRequests"`
		} `json:"detail"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("jira: decode dev-status: %w", err)
	}

	var links []PRLink
	for _, detail := range result.Detail {
		for _, pr := range detail.PullRequests {
			link := PRLink{
				ID:     pr.ID,
				Name:   pr.Name,
				URL:    pr.URL,
				Status: pr.Status,
			}
			if pr.Source.URL != "" {
				// Extract repo name from source URL (e.g., "https://github.com/org/repo" → "org/repo").
				link.Source = strings.TrimPrefix(pr.Source.URL, "https://github.com/")
			}
			links = append(links, link)
		}
	}
	return links, nil
}

func (c *Client) setAuth(req *http.Request) {
	req.SetBasicAuth(c.email, c.token)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
