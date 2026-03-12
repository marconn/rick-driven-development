// Package confluence provides a REST client for the Confluence Cloud API.
// Reads credentials from CONFLUENCE_URL, CONFLUENCE_EMAIL, CONFLUENCE_TOKEN
// environment variables.
package confluence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Client wraps an HTTP client configured for Confluence basic auth.
type Client struct {
	baseURL    string
	email      string
	token      string
	httpClient *http.Client
}

// NewClientFromEnv creates a Client from environment variables.
// Returns nil when vars are unset.
func NewClientFromEnv() *Client {
	baseURL := os.Getenv("CONFLUENCE_URL")
	email := os.Getenv("CONFLUENCE_EMAIL")
	if email == "" {
		email = os.Getenv("JIRA_EMAIL")
	}
	token := os.Getenv("CONFLUENCE_TOKEN")
	if token == "" {
		token = os.Getenv("JIRA_TOKEN")
	}

	if baseURL == "" || email == "" || token == "" {
		return nil
	}

	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		email:      email,
		token:      token,
		httpClient: &http.Client{},
	}
}

// NewClient creates a Client with explicit credentials.
func NewClient(baseURL, email, token string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		email:      email,
		token:      token,
		httpClient: &http.Client{},
	}
}

// Page represents a Confluence page with its content.
type Page struct {
	ID       string
	Title    string
	Body     string // HTML storage format
	Version  int
	SpaceKey string
}

// ReadPage fetches a Confluence page by ID.
func (c *Client) ReadPage(ctx context.Context, pageID string) (*Page, error) {
	url := fmt.Sprintf("%s/rest/api/content/%s?expand=body.storage,version,space", c.baseURL, pageID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("confluence: build request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("confluence: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("confluence: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Body  struct {
			Storage struct {
				Value string `json:"value"`
			} `json:"storage"`
		} `json:"body"`
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
		Space struct {
			Key string `json:"key"`
		} `json:"space"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("confluence: decode: %w", err)
	}

	return &Page{
		ID:       result.ID,
		Title:    result.Title,
		Body:     result.Body.Storage.Value,
		Version:  result.Version.Number,
		SpaceKey: result.Space.Key,
	}, nil
}

// UpdatePageSection updates a Confluence page, replacing content after a heading.
// Finds the heading marker and replaces everything between it and the next
// same-level heading (or end of document) with newContent.
func (c *Client) UpdatePageSection(ctx context.Context, page *Page, heading, newContent string) error {
	before, _, after, found := SplitAtHeading(page.Body, heading)
	if !found {
		return fmt.Errorf("confluence: heading %q not found in page %s", heading, page.ID)
	}

	updatedBody := before + newContent + after
	return c.updatePage(ctx, page.ID, page.Title, updatedBody, page.Version+1)
}

func (c *Client) updatePage(ctx context.Context, pageID, title, body string, version int) error {
	url := fmt.Sprintf("%s/rest/api/content/%s", c.baseURL, pageID)

	payload := map[string]any{
		"version": map[string]any{"number": version},
		"title":   title,
		"type":    "page",
		"body": map[string]any{
			"storage": map[string]any{
				"value":          body,
				"representation": "storage",
			},
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("confluence: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("confluence: build request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("confluence: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("confluence: update status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Client) setAuth(req *http.Request) {
	req.SetBasicAuth(c.email, c.token)
}

// SplitAtHeading splits HTML content at a heading containing the given text.
// Returns (before_heading, heading_section, after_section, found).
func SplitAtHeading(html, headingText string) (before, section, after string, found bool) {
	normalized := NormalizeEntities(strings.ToLower(html))
	headingLower := strings.ToLower(headingText)

	for _, tag := range []string{"<h1", "<h2", "<h3"} {
		idx := 0
		for {
			pos := strings.Index(normalized[idx:], tag)
			if pos == -1 {
				break
			}
			pos += idx

			closeTag := strings.Replace(tag, "<", "</", 1) + ">"
			endIdx := strings.Index(normalized[pos:], closeTag)
			if endIdx == -1 {
				idx = pos + len(tag)
				continue
			}
			endIdx += pos + len(closeTag)

			headingContent := normalized[pos:endIdx]
			if !strings.Contains(headingContent, headingLower) {
				idx = pos + len(tag)
				continue
			}

			origPos := findOriginalPos(html, tag, pos)
			if origPos == -1 {
				idx = pos + len(tag)
				continue
			}

			origCloseIdx := strings.Index(strings.ToLower(html[origPos:]), closeTag)
			if origCloseIdx == -1 {
				idx = pos + len(tag)
				continue
			}
			origEnd := origPos + origCloseIdx + len(closeTag)

			before = html[:origPos]
			rest := html[origPos:]

			nextLower := strings.ToLower(html[origEnd:])
			nextPos := strings.Index(nextLower, tag)
			if nextPos == -1 {
				section = rest
				after = ""
			} else {
				nextPos += origEnd
				section = html[origPos:nextPos]
				after = html[nextPos:]
			}
			return before, section, after, true
		}
	}
	return "", "", "", false
}

// findOriginalPos maps a position from normalized HTML back to original HTML.
func findOriginalPos(html, tag string, normalizedPos int) int {
	lower := strings.ToLower(html)
	normalizedLower := NormalizeEntities(lower)
	count := strings.Count(normalizedLower[:normalizedPos], tag)

	idx := 0
	for i := 0; i <= count; i++ {
		pos := strings.Index(lower[idx:], tag)
		if pos == -1 {
			return -1
		}
		if i == count {
			return idx + pos
		}
		idx += pos + len(tag)
	}
	return -1
}

// NormalizeEntities replaces common HTML entities with their character equivalents.
func NormalizeEntities(s string) string {
	r := strings.NewReplacer(
		"&eacute;", "é",
		"&oacute;", "ó",
		"&aacute;", "á",
		"&iacute;", "í",
		"&uacute;", "ú",
		"&ntilde;", "ñ",
		"&uuml;", "ü",
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", "\"",
		"&nbsp;", " ",
	)
	return r.Replace(s)
}

// ExtractTextContent strips HTML tags and returns plain text.
func ExtractTextContent(html string) string {
	var result strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			result.WriteRune(r)
		}
	}
	return strings.TrimSpace(result.String())
}
