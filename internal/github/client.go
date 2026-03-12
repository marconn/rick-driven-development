package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client is a lightweight GitHub REST API client.
type Client struct {
	baseURL    string // "https://api.github.com" for github.com
	token      string
	httpClient *http.Client
}

// NewClient creates a GitHub API client. Token should be a Personal Access Token
// or a GitHub App installation token with pull request write permissions.
func NewClient(token string) *Client {
	return &Client{
		baseURL:    "https://api.github.com",
		token:      token,
		httpClient: &http.Client{},
	}
}

// NewClientWithBase creates a client for GitHub Enterprise with a custom base URL.
func NewClientWithBase(baseURL, token string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{},
	}
}

// PRComment represents a comment on a pull request.
type PRComment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}

// CreatePRComment posts a comment on a pull request.
// owner/repo is the repository, prNumber is the PR number.
func (c *Client) CreatePRComment(ctx context.Context, owner, repo string, prNumber int, body string) (*PRComment, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, prNumber)
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return nil, fmt.Errorf("github: marshal comment: %w", err)
	}
	respBody, err := c.post(ctx, path, payload)
	if err != nil {
		return nil, fmt.Errorf("github: create PR comment on %s/%s#%d: %w", owner, repo, prNumber, err)
	}
	var comment PRComment
	if err := json.Unmarshal(respBody, &comment); err != nil {
		return nil, fmt.Errorf("github: unmarshal comment: %w", err)
	}
	return &comment, nil
}

// GetPR retrieves a pull request to check its existence and state.
func (c *Client) GetPR(ctx context.Context, owner, repo string, prNumber int) (*PullRequest, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	respBody, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("github: get PR %s/%s#%d: %w", owner, repo, prNumber, err)
	}
	var pr PullRequest
	if err := json.Unmarshal(respBody, &pr); err != nil {
		return nil, fmt.Errorf("github: unmarshal PR: %w", err)
	}
	return &pr, nil
}

// PullRequest represents a GitHub pull request.
type PullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"` // "open", "closed"
	HTMLURL string `json:"html_url"`
	Head    PRRef  `json:"head"`
	Base    PRRef  `json:"base"`
}

// PRRef is a branch reference in a PR.
type PRRef struct {
	Ref  string    `json:"ref"`
	Repo PRRepoRef `json:"repo"`
}

// PRRepoRef is the repo within a PR ref.
type PRRepoRef struct {
	FullName string `json:"full_name"` // "owner/repo"
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	return c.doRequest(req)
}

func (c *Client) post(ctx context.Context, path string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	return c.doRequest(req)
}

func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func (c *Client) doRequest(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// User is a GitHub user reference.
type User struct {
	Login string `json:"login"`
}

// Review is a top-level PR review.
type Review struct {
	ID      int    `json:"id"`
	User    User   `json:"user"`
	Body    string `json:"body"`
	State   string `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED
	HTMLURL string `json:"html_url"`
}

// ReviewComment is an inline review comment on a PR diff.
type ReviewComment struct {
	ID        int    `json:"id"`
	User      User   `json:"user"`
	Body      string `json:"body"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
	DiffHunk  string `json:"diff_hunk"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
}

// GetPRReviews fetches all reviews on a PR.
// GET /repos/{owner}/{repo}/pulls/{pr}/reviews
func (c *Client) GetPRReviews(ctx context.Context, owner, repo string, prNumber int) ([]Review, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, prNumber)
	pages, err := c.getPaginated(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("github: get PR reviews %s/%s#%d: %w", owner, repo, prNumber, err)
	}
	reviews := make([]Review, 0, len(pages))
	for _, raw := range pages {
		var r Review
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("github: unmarshal review: %w", err)
		}
		reviews = append(reviews, r)
	}
	return reviews, nil
}

// GetPRReviewComments fetches all inline review comments on a PR.
// GET /repos/{owner}/{repo}/pulls/{pr}/comments
func (c *Client) GetPRReviewComments(ctx context.Context, owner, repo string, prNumber int) ([]ReviewComment, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments", owner, repo, prNumber)
	pages, err := c.getPaginated(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("github: get PR review comments %s/%s#%d: %w", owner, repo, prNumber, err)
	}
	comments := make([]ReviewComment, 0, len(pages))
	for _, raw := range pages {
		var rc ReviewComment
		if err := json.Unmarshal(raw, &rc); err != nil {
			return nil, fmt.Errorf("github: unmarshal review comment: %w", err)
		}
		comments = append(comments, rc)
	}
	return comments, nil
}

// GetPRDiff fetches the PR diff as unified diff text.
// GET /repos/{owner}/{repo}/pulls/{pr} with Accept: application/vnd.github.diff
func (c *Client) GetPRDiff(ctx context.Context, owner, repo string, prNumber int) (string, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	body, err := c.getWithAccept(ctx, path, "application/vnd.github.diff")
	if err != nil {
		return "", fmt.Errorf("github: get PR diff %s/%s#%d: %w", owner, repo, prNumber, err)
	}
	return string(body), nil
}

// getPaginated follows Link header pagination, collecting all pages into a single slice.
func (c *Client) getPaginated(ctx context.Context, path string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	for path != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		c.setAuth(req)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
		}
		var page []json.RawMessage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("unmarshal page: %w", err)
		}
		all = append(all, page...)
		path = nextPagePath(resp.Header.Get("Link"))
	}
	return all, nil
}

// getWithAccept makes a GET request with a custom Accept header, overriding the default.
func (c *Client) getWithAccept(ctx context.Context, path, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return c.doRequest(req)
}

// CheckRun represents a GitHub Actions check run result.
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`     // "queued", "in_progress", "completed"
	Conclusion string `json:"conclusion"` // "success", "failure", "cancelled", "skipped", etc.
	HTMLURL    string `json:"html_url"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

// CheckRunsResponse is the GitHub API response for check runs.
type CheckRunsResponse struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
}

// GetCheckRuns fetches check runs for a specific commit SHA or ref.
// GET /repos/{owner}/{repo}/commits/{ref}/check-runs
func (c *Client) GetCheckRuns(ctx context.Context, owner, repo, ref string) (*CheckRunsResponse, error) {
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs", owner, repo, ref)
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("github: check runs for %s: %w", ref, err)
	}
	var result CheckRunsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("github: check runs unmarshal: %w", err)
	}
	return &result, nil
}

// PRHead represents the HEAD commit info from a pull request.
type PRHead struct {
	SHA string `json:"sha"`
	Ref string `json:"ref"` // branch name
}

// GetPRHead fetches the HEAD SHA and branch name for a pull request.
// GET /repos/{owner}/{repo}/pulls/{prNumber}
func (c *Client) GetPRHead(ctx context.Context, owner, repo string, prNumber int) (*PRHead, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, prNumber)
	body, err := c.get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("github: get PR head: %w", err)
	}
	var pr struct {
		Head PRHead `json:"head"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("github: PR head unmarshal: %w", err)
	}
	return &pr.Head, nil
}

// nextPagePath extracts the path for rel="next" from a GitHub Link header.
// Returns empty string if no next page.
// Input example: <https://api.github.com/repos/owner/repo/pulls/1/reviews?page=2>; rel="next"
func nextPagePath(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}
	// Each entry is: <URL>; rel="TYPE"
	for _, part := range strings.Split(linkHeader, ",") {
		part = strings.TrimSpace(part)
		segments := strings.Split(part, ";")
		if len(segments) < 2 {
			continue
		}
		rawURL := strings.TrimSpace(segments[0])
		rawURL = strings.TrimPrefix(rawURL, "<")
		rawURL = strings.TrimSuffix(rawURL, ">")
		rel := strings.TrimSpace(segments[1])
		if rel != `rel="next"` {
			continue
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return ""
		}
		// Return path + query so callers can use it with c.baseURL + path.
		if u.RawQuery != "" {
			return u.Path + "?" + u.RawQuery
		}
		return u.Path
	}
	return ""
}
