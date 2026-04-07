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
			Description: "Read a Jira ticket's key fields: summary, description, status, assignee, story points, acceptance criteria, labels, components, linked issues.",
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
				},
				"required": []string{"ticket"},
			},
		},
		Handler: s.toolJiraRead,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_read_qa_steps",
			Description: "Read the QA Steps custom field from a Jira ticket. Defaults to customfield_10037 (override with JIRA_QA_STEPS_FIELD env or the field_id arg). Use this because rick_jira_read does not return custom fields.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{
						"type":        "string",
						"description": "Jira issue key (e.g., PROJ-12345).",
					},
					"field_id": map[string]any{
						"type":        "string",
						"description": "Custom field ID for QA Steps. Optional; defaults to JIRA_QA_STEPS_FIELD env or customfield_10037.",
					},
				},
				"required": []string{"ticket"},
			},
		},
		Handler: s.toolJiraReadQASteps,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_write",
			Description: "Update a field on a Jira ticket (description, story_points, labels, custom fields).",
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
				},
				"required": []string{"ticket", "field_name", "value"},
			},
		},
		Handler: s.toolJiraWrite,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_transition",
			Description: "Transition a Jira ticket to a new status (e.g., TO DO -> IN DEVELOPMENT -> WF PEER REVIEW).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{
						"type":        "string",
						"description": "Jira issue key.",
					},
					"status": map[string]any{
						"type":        "string",
						"description": "Target status name (e.g., 'IN DEVELOPMENT', 'WF PEER REVIEW').",
					},
				},
				"required": []string{"ticket", "status"},
			},
		},
		Handler: s.toolJiraTransition,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_comment",
			Description: "Add a comment to a Jira ticket. The body is parsed as Markdown and converted to ADF, so headings, lists, tables, code blocks, links, and emphasis render natively in Jira.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{
						"type":        "string",
						"description": "Jira issue key.",
					},
					"comment": map[string]any{
						"type":        "string",
						"description": "Comment body in Markdown (GFM). Supports headings, bullet/ordered lists, tables, fenced code blocks, links, bold/italic, and inline code.",
					},
				},
				"required": []string{"ticket", "comment"},
			},
		},
		Handler: s.toolJiraComment,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_epic_issues",
			Description: "List all child issues of a Jira epic with status, assignee, story points, and labels.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic": map[string]any{
						"type":        "string",
						"description": "Epic issue key.",
					},
					"include_closed": map[string]any{
						"type":    "boolean",
						"default": true,
					},
				},
				"required": []string{"epic"},
			},
		},
		Handler: s.toolJiraEpicIssues,
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
			Name:        "rick_jira_set_microservice",
			Description: "Set the Microservice field on a Jira ticket. Falls back to adding a repo:<name> label if the microservice option does not exist.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{
						"type":        "string",
						"description": "Jira issue key.",
					},
					"microservice": map[string]any{
						"type":        "string",
						"description": "Microservice/repo name (e.g., 'backend', 'frontend').",
					},
				},
				"required": []string{"ticket", "microservice"},
			},
		},
		Handler: s.toolJiraSetMicroservice,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_pr_links",
			Description: "Get GitHub pull request links associated with a Jira issue via the GitHub integration.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ticket": map[string]any{
						"type":        "string",
						"description": "Jira issue key (e.g., PROJ-12345).",
					},
				},
				"required": []string{"ticket"},
			},
		},
		Handler: s.toolJiraPRLinks,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_link",
			Description: "Create an issue link between two Jira tickets (Blocks, Relates to, etc).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"from_ticket": map[string]any{
						"type":        "string",
						"description": "Source issue key.",
					},
					"to_ticket": map[string]any{
						"type":        "string",
						"description": "Target issue key.",
					},
					"link_type": map[string]any{
						"type":        "string",
						"default":     "Blocks",
						"description": "Link type name.",
					},
				},
				"required": []string{"from_ticket", "to_ticket"},
			},
		},
		Handler: s.toolJiraLink,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_delete_link",
			Description: "Delete an issue link between two Jira tickets by link ID. Use rick_jira_read to find link IDs first.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"link_id": map[string]any{
						"type":        "string",
						"description": "The issue link ID to delete.",
					},
				},
				"required": []string{"link_id"},
			},
		},
		Handler: s.toolJiraDeleteLink,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_jira_create",
			Description: "Create a new Jira issue (Task, Bug, Story, Epic, Sub-task).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{
						"type":        "string",
						"description": "Issue summary / title.",
					},
					"issue_type": map[string]any{
						"type":        "string",
						"description": "Issue type: Task, Bug, Story, Epic, Sub-task.",
						"default":     "Task",
					},
					"project": map[string]any{
						"type":        "string",
						"description": "Project key (e.g., PROJ). Defaults to JIRA_PROJECT env.",
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
						"description": "Priority name (e.g., High, Medium, Low).",
					},
					"assigned_team": map[string]any{
						"type":        "string",
						"description": "Team ID to override the default JIRA_TEAM_ID (numeric ID for the Assigned Team field).",
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
		return fmt.Errorf("Jira client not configured (set JIRA_URL, JIRA_EMAIL, JIRA_TOKEN)")
	}
	return nil
}

type jiraReadArgs struct {
	Ticket string   `json:"ticket"`
	Fields []string `json:"fields"`
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

	return result, nil
}

type jiraReadQAStepsArgs struct {
	Ticket  string `json:"ticket"`
	FieldID string `json:"field_id"`
}

func (s *Server) toolJiraReadQASteps(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraReadQAStepsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Ticket == "" {
		return nil, fmt.Errorf("ticket is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	fieldID := args.FieldID
	if fieldID == "" {
		fieldID = os.Getenv("JIRA_QA_STEPS_FIELD")
	}
	if fieldID == "" {
		fieldID = defaultQAStepsField
	}

	issue, err := s.deps.Jira.FetchRawIssue(ctx, args.Ticket)
	if err != nil {
		return nil, fmt.Errorf("fetch issue: %w", err)
	}

	rawField, present := issue.Fields[fieldID]
	result := map[string]any{
		"ticket":   args.Ticket,
		"field_id": fieldID,
		"present":  present && len(rawField) > 0 && string(rawField) != "null",
	}

	if result["present"].(bool) {
		result["qa_steps"] = jira.ExtractTextField(rawField)
	} else {
		result["qa_steps"] = ""
	}

	return result, nil
}

type jiraWriteArgs struct {
	Ticket    string `json:"ticket"`
	FieldName string `json:"field_name"`
	Value     any    `json:"value"`
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

func (s *Server) toolJiraWrite(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraWriteArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Ticket == "" || args.FieldName == "" {
		return nil, fmt.Errorf("ticket and field_name are required")
	}
	if args.Value == nil {
		return nil, fmt.Errorf("value is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

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

	return map[string]any{
		"ticket":  args.Ticket,
		"field":   args.FieldName,
		"updated": true,
	}, nil
}

type jiraTransitionArgs struct {
	Ticket string `json:"ticket"`
	Status string `json:"status"`
}

func (s *Server) toolJiraTransition(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraTransitionArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Ticket == "" || args.Status == "" {
		return nil, fmt.Errorf("ticket and status are required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	if err := s.deps.Jira.TransitionIssue(ctx, args.Ticket, args.Status); err != nil {
		return nil, fmt.Errorf("transition: %w", err)
	}

	return map[string]any{
		"ticket":       args.Ticket,
		"status":       args.Status,
		"transitioned": true,
	}, nil
}

type jiraCommentArgs struct {
	Ticket  string `json:"ticket"`
	Comment string `json:"comment"`
}

func (s *Server) toolJiraComment(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraCommentArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Ticket == "" || args.Comment == "" {
		return nil, fmt.Errorf("ticket and comment are required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	if err := s.deps.Jira.AddComment(ctx, args.Ticket, args.Comment); err != nil {
		return nil, fmt.Errorf("add comment: %w", err)
	}

	return map[string]any{
		"ticket":  args.Ticket,
		"commented": true,
	}, nil
}

type jiraSetMicroserviceArgs struct {
	Ticket       string `json:"ticket"`
	Microservice string `json:"microservice"`
}

func (s *Server) toolJiraSetMicroservice(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraSetMicroserviceArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Ticket == "" || args.Microservice == "" {
		return nil, fmt.Errorf("ticket and microservice are required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	method, err := s.deps.Jira.SetMicroservice(ctx, args.Ticket, args.Microservice)
	if err != nil {
		return nil, fmt.Errorf("set microservice: %w", err)
	}

	return map[string]any{
		"ticket":       args.Ticket,
		"microservice": args.Microservice,
		"method":       method,
		"updated":      true,
	}, nil
}

type jiraEpicArgs struct {
	Epic          string `json:"epic"`
	IncludeClosed *bool  `json:"include_closed"`
}

func (s *Server) toolJiraEpicIssues(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraEpicArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Epic == "" {
		return nil, fmt.Errorf("epic is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	includeClosed := true
	if args.IncludeClosed != nil {
		includeClosed = *args.IncludeClosed
	}

	children, err := s.deps.Jira.FetchEpicChildren(ctx, args.Epic, includeClosed)
	if err != nil {
		return nil, fmt.Errorf("fetch epic children: %w", err)
	}

	return map[string]any{
		"epic":   args.Epic,
		"issues": children,
		"count":  len(children),
	}, nil
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

type jiraLinkArgs struct {
	FromTicket string `json:"from_ticket"`
	ToTicket   string `json:"to_ticket"`
	LinkType   string `json:"link_type"`
}

func (s *Server) toolJiraLink(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraLinkArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.FromTicket == "" || args.ToTicket == "" {
		return nil, fmt.Errorf("from_ticket and to_ticket are required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}
	if args.LinkType == "" {
		args.LinkType = "Blocks"
	}

	if err := s.deps.Jira.LinkIssuesWithType(ctx, args.FromTicket, args.ToTicket, args.LinkType); err != nil {
		return nil, fmt.Errorf("link issues: %w", err)
	}

	return map[string]any{
		"from":    args.FromTicket,
		"to":      args.ToTicket,
		"type":    args.LinkType,
		"linked":  true,
	}, nil
}

type jiraDeleteLinkArgs struct {
	LinkID string `json:"link_id"`
}

func (s *Server) toolJiraDeleteLink(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraDeleteLinkArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.LinkID == "" {
		return nil, fmt.Errorf("link_id is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	if err := s.deps.Jira.DeleteIssueLink(ctx, args.LinkID); err != nil {
		return nil, fmt.Errorf("delete link: %w", err)
	}

	return map[string]any{
		"link_id": args.LinkID,
		"deleted": true,
	}, nil
}

type jiraPRLinksArgs struct {
	Ticket string `json:"ticket"`
}

func (s *Server) toolJiraPRLinks(ctx context.Context, raw json.RawMessage) (any, error) {
	var args jiraPRLinksArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Ticket == "" {
		return nil, fmt.Errorf("ticket is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	links, err := s.deps.Jira.FetchPRLinks(ctx, args.Ticket)
	if err != nil {
		return nil, fmt.Errorf("fetch PR links: %w", err)
	}

	return map[string]any{
		"ticket": args.Ticket,
		"prs":    links,
		"count":  len(links),
	}, nil
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
