package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/jira"
)

// defaultQAStepsField is the Jira custom field ID that stores QA Steps in the
// Huli/Team Rocket instance. Override with the JIRA_QA_STEPS_FIELD env var.
const defaultQAStepsField = "customfield_10037"

func (s *Server) registerJiraTools() {

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_read",
			Description: "Read a Jira ticket's key fields: summary, description, status, assignee, story points, acceptance criteria, labels, components, linked issues. Optionally fetch qa_steps, pr_links, and epic_issues in the same call.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{
						"type":        "string",
						"description": "Jira issue key (e.g., PROJ-12345).",
					},
					"fields": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Specific fields to return. Omit for all.",
					},
					"include_qa_steps": map[string]any{
						"type":        "boolean",
						"default":     false,
						"description": "If true, fetches the QA Steps custom field.",
					},
					"include_pr_links": map[string]any{
						"type":        "boolean",
						"default":     false,
						"description": "If true, fetches associated GitHub pull request links.",
					},
					"include_epic_issues": map[string]any{
						"type":        "boolean",
						"default":     false,
						"description": "If the ticket is an Epic, fetches its child issues.",
					},
				},
				"required": []string{"ticket"},
			},
		},
		Handler: s.toolJiraRead,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_write",
			Description: "Update fields, add comments, or transition a Jira ticket. For fields use field_name/value. For comments use comment. For status transitions use status. For setting microservice use microservice.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{
						"type":        "string",
						"description": "Jira issue key.",
					},
					"field_name": map[string]any{
						"type":        "string",
						"description": "Field to update (description, story_points, labels, or a custom field ID like customfield_10035).",
					},
					"value": map[string]any{
						"description": "New value for the field.",
					},
					"comment": map[string]any{
						"type":        "string",
						"description": "Comment body in Markdown to add.",
					},
					"status": map[string]any{
						"type":        "string",
						"description": "Target status name to transition to.",
					},
					"microservice": map[string]any{
						"type":        "string",
						"description": "Microservice/repo name to set.",
					},
				},
				"required": []string{"ticket"},
			},
		},
		Handler: s.toolJiraWrite,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_search",
			Description: "Run a JQL query and return matching issues.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"jql": map[string]any{
						"type":        "string",
						"description": "JQL query string.",
					},
					"fields": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Fields to return.",
					},
					"limit": map[string]any{
						"type":    "integer",
						"default": 50,
					},
				},
				"required": []string{"jql"},
			},
		},
		Handler: s.toolJiraSearch,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_manage_links",
			Description: "Create or delete issue links. To create: provide from_ticket, to_ticket, and link_type. To delete: provide link_id.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type": "string",
						"enum": []string{"create", "delete"},
					},
					"from_ticket": map[string]any{
						"type":        "string",
						"description": "Active side of the link (for create).",
					},
					"to_ticket": map[string]any{
						"type":        "string",
						"description": "Passive side of the link (for create).",
					},
					"link_type": map[string]any{
						"type":        "string",
						"default":     "Blocks",
						"description": "Link type name (for create).",
					},
					"link_id": map[string]any{
						"type":        "string",
						"description": "ID of the link to delete.",
					},
				},
				"required": []string{"action"},
			},
		},
		Handler: s.toolJiraManageLinks,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_create",
			Description: "Create a new Jira issue (Task, Bug, Story, Epic, etc). Returns the new key.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{
						"type":        "string",
						"description": "Issue summary/title.",
					},
					"issue_type": map[string]any{
						"type":        "string",
						"description": "Issue type: Task, Bug, Story, Epic, Sub-task.",
						"default":     "Task",
					},
					"project": map[string]any{
						"type":        "string",
						"description": "Project key (e.g., HULI). Defaults to JIRA_PROJECT env var. Required if JIRA_PROJECT is unset.",
					},
					"description": map[string]any{
						"type":        "string",
						"description": "Issue description (markdown).",
					},
					"epic_key": map[string]any{
						"type":        "string",
						"description": "Parent epic key (e.g., PROJ-100).",
					},
					"story_points": map[string]any{
						"type":        "number",
						"description": "Story points (Fibonacci).",
					},
					"labels": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Labels to set on the issue.",
					},
					"components": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Component names.",
					},
					"priority": map[string]any{
						"type":        "string",
						"description": "Priority name as configured in your Jira instance (run a JQL search to discover valid values).",
					},
					"assigned_team": map[string]any{
						"type":        "string",
						"description": "Numeric team ID for the Assigned Team field. Defaults to JIRA_TEAM_ID env var. Known IDs: 10571 (Team Rocket), 10204 (Team Darwin). Required if JIRA_TEAM_ID is unset.",
					},
				},
				"required": []string{"summary"},
			},
		},
		Handler: s.toolJiraCreate,
	})
}

// --- Handlers ---

func (s *Server) requireJira() error {
	if s.deps.Jira == nil {
		return fmt.Errorf("jira client not configured (set JIRA_URL, JIRA_EMAIL, JIRA_TOKEN)")
	}
	return nil
}

type jiraReadArgs struct {
	Ticket            string   `json:"ticket"`
	Fields            []string `json:"fields"`
	IncludeQASteps    bool     `json:"include_qa_steps"`
	IncludePRLinks    bool     `json:"include_pr_links"`
	IncludeEpicIssues bool     `json:"include_epic_issues"`
}

func (s *Server) toolJiraRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraReadArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Ticket == "" {
		return nil, fmt.Errorf("ticket is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	issue, err := s.deps.Jira.FetchIssue(ctx, args.Ticket)
	if err != nil {
		return nil, fmt.Errorf("fetch issue: %w", err)
	}

	result := map[string]any{
		"key":        issue.Key,
		"summary":    issue.Fields.Summary,
		"status":     issue.Fields.Status.Name,
		"labels":     issue.Fields.Labels,
		"components": issue.ComponentNames(),
	}

	// Repo: microservice field first, then repo: label fallback.
	if ms := issue.MicroserviceName(); ms != "" {
		result["microservice"] = ms
		result["repo"] = ms
	} else {
		for _, l := range issue.Fields.Labels {
			if after, ok := strings.CutPrefix(l, "repo:"); ok {
				result["repo"] = after
				break
			}
		}
	}

	desc := jira.ADFToPlainText(issue.Fields.Description)
	if desc != "" {
		result["description"] = desc
	}

	ac := jira.ADFToPlainText(issue.Fields.AcceptanceCriteria10035)
	if ac == "" {
		ac = jira.ADFToPlainText(issue.Fields.AcceptanceCriteria10036)
	}
	if ac != "" {
		result["acceptance_criteria"] = ac
	}

	// Fetch links separately since FetchIssue doesn't include them.
	if links, linkErr := s.deps.Jira.FetchIssueLinks(ctx, args.Ticket); linkErr == nil && len(links) > 0 {
		result["links"] = links
	}

	if args.IncludeQASteps {
		fieldID := os.Getenv("JIRA_QA_STEPS_FIELD")
		if fieldID == "" {
			fieldID = defaultQAStepsField
		}
		rawIssue, err := s.deps.Jira.FetchRawIssue(ctx, args.Ticket)
		if err == nil {
			rawField, present := rawIssue.Fields[fieldID]
			if present && len(rawField) > 0 && string(rawField) != "null" {
				result["qa_steps"] = jira.ExtractTextField(rawField)
			} else {
				result["qa_steps"] = ""
			}
		}
	}

	if args.IncludePRLinks {
		links, err := s.deps.Jira.FetchPRLinks(ctx, args.Ticket)
		if err == nil {
			result["pr_links"] = links
		}
	}

	if args.IncludeEpicIssues {
		children, err := s.deps.Jira.FetchEpicChildren(ctx, args.Ticket, true)
		if err == nil {
			result["epic_issues"] = children
		}
	}

	return result, nil
}

// knownFieldMap maps friendly names to Jira field IDs.
var knownFieldMap = map[string]string{
	"description":         "description",
	"story_points":        "customfield_10004",
	"labels":              "labels",
	"acceptance_criteria": "customfield_10035",
	"microservice":        "customfield_11538",
}

// selectFields are Jira custom fields that require {"value": "..."} wrapping.
var selectFields = map[string]bool{
	"customfield_11538": true, // Microservice
}

// numberFields are Jira fields that require a numeric value (not a string).
var numberFields = map[string]bool{
	"customfield_10004": true, // Story Points
}

type jiraWriteArgs struct {
	Ticket       string `json:"ticket"`
	FieldName    string `json:"field_name"`
	Value        any    `json:"value"`
	Comment      string `json:"comment"`
	Status       string `json:"status"`
	Microservice string `json:"microservice"`
}

func (s *Server) toolJiraWrite(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraWriteArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Ticket == "" {
		return nil, fmt.Errorf("ticket is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	result := map[string]any{
		"ticket": args.Ticket,
	}

	if args.FieldName != "" && args.Value != nil {
		fieldID := args.FieldName
		if mapped, ok := knownFieldMap[args.FieldName]; ok {
			fieldID = mapped
		}

		// For description, convert markdown to ADF.
		value := args.Value
		if fieldID == "description" {
			if str, ok := value.(string); ok {
				value = jira.MarkdownToADF(str)
			}
		}

		// Select fields need {"value": "..."} wrapping.
		if selectFields[fieldID] {
			if str, ok := value.(string); ok {
				value = map[string]any{"value": str}
			}
		}

		// Number fields must be numeric — coerce string to float64.
		if numberFields[fieldID] {
			if str, ok := value.(string); ok {
				n, err := strconv.ParseFloat(str, 64)
				if err != nil {
					return nil, fmt.Errorf("field %s requires a number, got %q", args.FieldName, str)
				}
				value = n
			}
		}

		if err := s.deps.Jira.UpdateField(ctx, args.Ticket, fieldID, value); err != nil {
			return nil, fmt.Errorf("update field: %w", err)
		}
		result["field_updated"] = args.FieldName
	}

	if args.Comment != "" {
		if err := s.deps.Jira.AddComment(ctx, args.Ticket, args.Comment); err != nil {
			return nil, fmt.Errorf("add comment: %w", err)
		}
		result["commented"] = true
	}

	if args.Status != "" {
		if err := s.deps.Jira.TransitionIssue(ctx, args.Ticket, args.Status); err != nil {
			return nil, fmt.Errorf("transition: %w", err)
		}
		result["transitioned"] = args.Status
	}

	if args.Microservice != "" {
		method, err := s.deps.Jira.SetMicroservice(ctx, args.Ticket, args.Microservice)
		if err != nil {
			return nil, fmt.Errorf("set microservice: %w", err)
		}
		result["microservice_set"] = args.Microservice
		result["microservice_method"] = method
	}

	return result, nil
}

type jiraSearchArgs struct {
	JQL    string   `json:"jql"`
	Fields []string `json:"fields"`
	Limit  int      `json:"limit"`
}

func (s *Server) toolJiraSearch(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraSearchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.JQL == "" {
		return nil, fmt.Errorf("jql is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}
	if args.Limit <= 0 {
		args.Limit = 50
	}

	result, err := s.deps.Jira.Search(ctx, args.JQL, args.Limit)
	if err != nil {
		return nil, fmt.Errorf("jira search: %w", err)
	}

	issues := make([]map[string]any, 0, len(result.Issues))
	for _, iss := range result.Issues {
		issues = append(issues, map[string]any{
			"key":     iss.Key,
			"summary": iss.Fields.Summary,
		})
	}

	return map[string]any{
		"total":  result.Total,
		"issues": issues,
		"count":  len(issues),
	}, nil
}

type jiraManageLinksArgs struct {
	Action     string `json:"action"`
	FromTicket string `json:"from_ticket"`
	ToTicket   string `json:"to_ticket"`
	LinkType   string `json:"link_type"`
	LinkID     string `json:"link_id"`
}

func (s *Server) toolJiraManageLinks(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraManageLinksArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	switch args.Action {
	case "create":
		if args.FromTicket == "" || args.ToTicket == "" {
			return nil, fmt.Errorf("from_ticket and to_ticket are required for create action")
		}
		if args.LinkType == "" {
			args.LinkType = "Blocks"
		}
		if err := s.deps.Jira.LinkIssuesWithType(ctx, args.FromTicket, args.ToTicket, args.LinkType); err != nil {
			return nil, fmt.Errorf("link issues: %w", err)
		}
		return map[string]any{
			"action":  "create",
			"from":    args.FromTicket,
			"to":      args.ToTicket,
			"type":    args.LinkType,
			"created": true,
		}, nil
	case "delete":
		if args.LinkID == "" {
			return nil, fmt.Errorf("link_id is required for delete action")
		}
		if err := s.deps.Jira.DeleteIssueLink(ctx, args.LinkID); err != nil {
			return nil, fmt.Errorf("delete link: %w", err)
		}
		return map[string]any{
			"action":  "delete",
			"link_id": args.LinkID,
			"deleted": true,
		}, nil
	default:
		return nil, fmt.Errorf("invalid action: %q", args.Action)
	}
}

type jiraCreateArgs struct {
	Summary      string   `json:"summary"`
	IssueType    string   `json:"issue_type"`
	Project      string   `json:"project"`
	Description  string   `json:"description"`
	EpicKey      string   `json:"epic_key"`
	StoryPoints  float64  `json:"story_points"`
	Labels       []string `json:"labels"`
	Components   []string `json:"components"`
	Priority     string   `json:"priority"`
	AssignedTeam string   `json:"assigned_team"`
}

func (s *Server) toolJiraCreate(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraCreateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Summary == "" {
		return nil, fmt.Errorf("summary is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}
	if args.IssueType == "" {
		args.IssueType = "Task"
	}
	if args.Project == "" && os.Getenv("JIRA_PROJECT") == "" {
		return nil, fmt.Errorf("project is required — set JIRA_PROJECT env var or pass a project key")
	}
	if args.AssignedTeam == "" && os.Getenv("JIRA_TEAM_ID") == "" {
		return nil, fmt.Errorf("assigned_team is required — set JIRA_TEAM_ID env var or pass a team ID (e.g., 10571 for Team Rocket)")
	}

	var opts []jira.CreateOption
	if args.Project != "" {
		opts = append(opts, jira.WithProject(args.Project))
	}
	if args.EpicKey != "" {
		opts = append(opts, jira.WithEpicLink(args.EpicKey))
	}
	if args.StoryPoints > 0 {
		opts = append(opts, jira.WithStoryPoints(args.StoryPoints))
	}
	if len(args.Labels) > 0 {
		opts = append(opts, jira.WithLabels(args.Labels))
	}
	if len(args.Components) > 0 {
		opts = append(opts, jira.WithComponents(args.Components))
	}
	if args.Priority != "" {
		opts = append(opts, jira.WithPriority(args.Priority))
	}
	if args.AssignedTeam != "" {
		opts = append(opts, jira.WithTeam(args.AssignedTeam))
	}

	key, err := s.deps.Jira.CreateIssue(ctx, args.IssueType, args.Summary, args.Description, opts...)
	if err != nil {
		return nil, fmt.Errorf("create issue: %w", err)
	}

	return map[string]any{
		"key":     key,
		"url":     s.deps.Jira.BaseURL() + "/browse/" + key,
		"created": true,
	}, nil
}
