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
	defer func() { _ = resp.Body.Close() }()
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

// --- Projects V2 ---

// projectItemsQuery pages through a Projects V2 board and returns each item's
// content (Issue / PullRequest / DraftIssue) plus its text-typed custom field
// values. The wave planner consumes `Issue` items, skips `DraftIssue` with a
// diagnostic, and treats `PullRequest` as skipped (wave planning is issue-
// centric).
//
// Custom field values are filtered to text type because the "Depends on"
// convention stores dependencies as a free-form comma-separated list of issue
// refs. Single-select/number/date fields are ignored. This query uses inline
// fragments on both `Organization` and `User` so the caller can pick whichever
// owner type matches their project.
const projectItemsQuery = `
query ProjectItems($login: String!, $number: Int!, $cursor: String) {
  organization(login: $login) {
    projectV2(number: $number) {
      title
      items(first: 100, after: $cursor) {
        pageInfo { endCursor hasNextPage }
        nodes { ...ItemFields }
      }
    }
  }
  user(login: $login) {
    projectV2(number: $number) {
      title
      items(first: 100, after: $cursor) {
        pageInfo { endCursor hasNextPage }
        nodes { ...ItemFields }
      }
    }
  }
}
fragment ItemFields on ProjectV2Item {
  id
  content {
    __typename
    ... on Issue {
      number
      title
      body
      state
      repository { nameWithOwner }
      labels(first: 20) { nodes { name } }
    }
    ... on PullRequest {
      number
      title
      state
      repository { nameWithOwner }
    }
    ... on DraftIssue {
      title
    }
  }
  fieldValues(first: 20) {
    nodes {
      __typename
      ... on ProjectV2ItemFieldTextValue {
        text
        field { ... on ProjectV2FieldCommon { name } }
      }
    }
  }
}
`

// ProjectV2Item is one item on a Projects V2 board, plus its text field values.
type ProjectV2Item struct {
	ID      string `json:"id"`
	Content struct {
		Typename     string `json:"__typename"`
		Number       int    `json:"number"`
		Title        string `json:"title"`
		Body         string `json:"body"`
		State        string `json:"state"`
		Repository   struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
		Labels struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"labels"`
	} `json:"content"`
	FieldValues struct {
		Nodes []ProjectV2FieldValue `json:"nodes"`
	} `json:"fieldValues"`
}

// ProjectV2FieldValue is a text-typed custom field value on a project item.
// We only decode the text variant — other field types (select, number, date)
// are not relevant to wave dependency resolution.
type ProjectV2FieldValue struct {
	Typename string `json:"__typename"`
	Text     string `json:"text"`
	Field    struct {
		Name string `json:"name"`
	} `json:"field"`
}

// ProjectV2Response is the projectItemsQuery response. Either Organization or
// User is populated (the non-matching owner variant is nil) — the planner
// picks whichever one returned a project.
type ProjectV2Response struct {
	Organization *ProjectV2Scope `json:"organization"`
	User         *ProjectV2Scope `json:"user"`
}

// ProjectV2Scope is the inner projectV2 shape shared between org and user
// owners.
type ProjectV2Scope struct {
	ProjectV2 *ProjectV2 `json:"projectV2"`
}

// ProjectV2 is a Projects V2 board with the current page of items.
type ProjectV2 struct {
	Title string `json:"title"`
	Items struct {
		PageInfo struct {
			EndCursor   string `json:"endCursor"`
			HasNextPage bool   `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []ProjectV2Item `json:"nodes"`
	} `json:"items"`
}

// FetchProjectV2Items pages through a Projects V2 board and returns every
// item. `login` is the org or user that owns the project. The query tries
// both organization and user scope in one round-trip and returns whichever
// one matched — callers don't need to know which kind of owner it is.
// State normalization: GraphQL returns uppercase states; we lowercase for
// parity with the REST paths.
func (c *Client) FetchProjectV2Items(ctx context.Context, login string, number int) (*ProjectV2, error) {
	var all []ProjectV2Item
	var title string
	cursor := ""
	for {
		vars := map[string]any{"login": login, "number": number}
		if cursor != "" {
			vars["cursor"] = cursor
		} else {
			vars["cursor"] = nil
		}
		var resp ProjectV2Response
		if err := c.GraphQLQuery(ctx, projectItemsQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("projects v2: %w", err)
		}
		var page *ProjectV2
		switch {
		case resp.Organization != nil && resp.Organization.ProjectV2 != nil:
			page = resp.Organization.ProjectV2
		case resp.User != nil && resp.User.ProjectV2 != nil:
			page = resp.User.ProjectV2
		default:
			return nil, fmt.Errorf("projects v2: no project found for %s/projects/%d (requires read:project token scope or non-existent board)", login, number)
		}
		if title == "" {
			title = page.Title
		}
		for i := range page.Items.Nodes {
			node := &page.Items.Nodes[i]
			node.Content.State = strings.ToLower(node.Content.State)
			all = append(all, *node)
		}
		if !page.Items.PageInfo.HasNextPage {
			break
		}
		cursor = page.Items.PageInfo.EndCursor
	}
	return &ProjectV2{
		Title: title,
		Items: struct {
			PageInfo struct {
				EndCursor   string `json:"endCursor"`
				HasNextPage bool   `json:"hasNextPage"`
			} `json:"pageInfo"`
			Nodes []ProjectV2Item `json:"nodes"`
		}{
			Nodes: all,
		},
	}, nil
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
