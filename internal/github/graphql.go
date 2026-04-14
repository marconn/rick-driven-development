package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GraphQLBaseURL returns the GraphQL endpoint for this client. REST clients
// target api.github.com; GraphQL is served at api.github.com/graphql.
func (c *Client) GraphQLBaseURL() string {
	// Allow the same "/graphql" suffix to work for Enterprise hosts where the
	// caller passed a base path like "https://ghe.example.com/api/v3" — those
	// expose GraphQL under "/api/graphql".
	if strings.HasSuffix(c.baseURL, "/api/v3") {
		return strings.TrimSuffix(c.baseURL, "/api/v3") + "/api/graphql"
	}
	if strings.HasSuffix(c.baseURL, "api.github.com") {
		return c.baseURL + "/graphql"
	}
	return c.baseURL + "/graphql"
}

// GraphQLQuery executes an arbitrary GraphQL query with variables and
// unmarshals data into `out` (typically a pointer to a struct that matches
// the query's response shape).
func (c *Client) GraphQLQuery(ctx context.Context, query string, variables map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return fmt.Errorf("github graphql: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.GraphQLBaseURL(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github graphql: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("github graphql: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("github graphql: HTTP %d: %s", resp.StatusCode, string(body))
	}
	// GraphQL wraps errors inside the body even with HTTP 200.
	var env struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("github graphql: unmarshal envelope: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("github graphql: server errors: %s", string(body))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("github graphql: unmarshal data: %w", err)
	}
	return nil
}

// --- Wave-planning GraphQL helpers ---

// waveParentQuery fetches parent issue body + sub-issues (title, state,
// labels, PR marker, repository) + timeline cross-references in a single
// round-trip. Used by the wave planner when RICK_GITHUB_GRAPHQL=1.
const waveParentQuery = `
query WaveParent($owner: String!, $name: String!, $number: Int!, $subLimit: Int = 50, $tlLimit: Int = 50) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      number
      title
      body
      state
      subIssues(first: $subLimit) {
        nodes {
          number
          title
          state
          body
          repository { nameWithOwner }
          labels(first: 20) { nodes { name } }
          timelineItems(first: $tlLimit, itemTypes: [CROSS_REFERENCED_EVENT]) {
            nodes {
              __typename
              ... on CrossReferencedEvent {
                source {
                  __typename
                  ... on PullRequest { number state repository { nameWithOwner } }
                }
              }
            }
          }
        }
      }
    }
  }
}
`

// WaveParentResponse mirrors the GraphQL data for waveParentQuery; exposed so
// the mcp planner can consume it without redefining the shape.
type WaveParentResponse struct {
	Repository struct {
		Issue struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
			State  string `json:"state"`
			SubIssues struct {
				Nodes []WaveParentSubIssue `json:"nodes"`
			} `json:"subIssues"`
		} `json:"issue"`
	} `json:"repository"`
}

// WaveParentSubIssue is one sub-issue fetched via GraphQL.
type WaveParentSubIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Body   string `json:"body"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	TimelineItems struct {
		Nodes []WaveParentTimelineItem `json:"nodes"`
	} `json:"timelineItems"`
}

// WaveParentTimelineItem is a cross-referenced PR node.
type WaveParentTimelineItem struct {
	Typename string `json:"__typename"`
	Source   struct {
		Typename string `json:"__typename"`
		Number   int    `json:"number"`
		State    string `json:"state"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	} `json:"source"`
}

// FetchWaveParent executes waveParentQuery and returns the decoded response.
// State-normalization: GraphQL returns uppercase states ("OPEN"/"CLOSED"/
// "MERGED"); callers downstream expect lowercase "open"/"closed", so we
// rewrite them here.
func (c *Client) FetchWaveParent(ctx context.Context, owner, name string, number int) (*WaveParentResponse, error) {
	vars := map[string]any{"owner": owner, "name": name, "number": number}
	var resp WaveParentResponse
	if err := c.GraphQLQuery(ctx, waveParentQuery, vars, &resp); err != nil {
		return nil, err
	}
	resp.Repository.Issue.State = strings.ToLower(resp.Repository.Issue.State)
	for i := range resp.Repository.Issue.SubIssues.Nodes {
		n := &resp.Repository.Issue.SubIssues.Nodes[i]
		n.State = strings.ToLower(n.State)
		for j := range n.TimelineItems.Nodes {
			n.TimelineItems.Nodes[j].Source.State = strings.ToLower(n.TimelineItems.Nodes[j].Source.State)
		}
	}
	return &resp, nil
}
