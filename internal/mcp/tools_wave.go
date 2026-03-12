package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/jira"
)

func (s *Server) registerWaveTools() {

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_plan",
			Description: "Compute development waves from a Jira epic. Reads children and dependency links, performs topological sort, returns a wave schedule showing which tickets can be developed in parallel.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic": map[string]any{
						"type":        "string",
						"description": "Epic issue key.",
					},
				},
				"required": []string{"epic"},
			},
		},
		Handler: s.toolWavePlan,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_launch",
			Description: "Launch a wave of parallel development jobs. For each ticket: creates isolated workspace, starts a jira-dev workflow. Returns correlation IDs for monitoring.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic": map[string]any{
						"type":        "string",
						"description": "Epic issue key.",
					},
					"wave": map[string]any{
						"type":        "integer",
						"description": "Wave number to launch (from rick_wave_plan). Omit for next ready wave.",
					},
					"dag": map[string]any{
						"type":        "string",
						"default":     "jira-dev",
						"description": "Workflow DAG to use for each ticket.",
					},
					"tickets": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Override: specific ticket keys to launch (subset of wave).",
					},
					"dry_run": map[string]any{
						"type":    "boolean",
						"default": false,
					},
				},
				"required": []string{"epic"},
			},
		},
		Handler: s.toolWaveLaunch,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_status",
			Description: "Monitor the progress of a launched wave. Shows workflow status for each ticket plus aggregate view.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic": map[string]any{
						"type":        "string",
						"description": "Epic issue key.",
					},
					"wave": map[string]any{
						"type":        "integer",
						"description": "Wave number (optional, shows all waves if omitted).",
					},
				},
				"required": []string{"epic"},
			},
		},
		Handler: s.toolWaveStatus,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_cleanup",
			Description: "Remove all isolated workspaces for a completed wave.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic": map[string]any{
						"type":        "string",
						"description": "Epic issue key.",
					},
					"wave": map[string]any{
						"type":    "integer",
						"description": "Wave number to clean up.",
					},
					"force": map[string]any{
						"type":    "boolean",
						"default": false,
						"description": "Clean up even if some workflows are still running.",
					},
				},
				"required": []string{"epic"},
			},
		},
		Handler: s.toolWaveCleanup,
	})
}

// --- Wave computation ---

type wavePlanTicket struct {
	Key     string  `json:"key"`
	Summary string  `json:"summary"`
	Repo    string  `json:"repo,omitempty"`
	Status  string  `json:"status"`
	Points  float64 `json:"points,omitempty"`
}

type wavePlanWave struct {
	Wave      int              `json:"wave"`
	Tickets   []wavePlanTicket `json:"tickets"`
	Ready     bool             `json:"ready"`
	BlockedBy []string         `json:"blocked_by,omitempty"`
}

type wavePlanResult struct {
	Epic        string         `json:"epic"`
	Waves       []wavePlanWave `json:"waves"`
	TotalPoints float64        `json:"total_points"`
	Parallelism int            `json:"parallelism"`
}

func (s *Server) toolWavePlan(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Epic string `json:"epic"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Epic == "" {
		return nil, fmt.Errorf("epic is required")
	}
	return s.computeWavePlan(ctx, args.Epic)
}

// computeWavePlan is the internal implementation shared by wave tools.
func (s *Server) computeWavePlan(ctx context.Context, epic string) (wavePlanResult, error) {
	if err := s.requireJira(); err != nil {
		return wavePlanResult{}, err
	}

	children, err := s.deps.Jira.FetchEpicChildren(ctx, epic, true)
	if err != nil {
		return wavePlanResult{}, fmt.Errorf("fetch epic children: %w", err)
	}

	// Build dependency graph: key → set of keys that block it.
	deps := make(map[string]map[string]bool)
	childKeys := make(map[string]bool)
	for _, c := range children {
		childKeys[c.Key] = true
		deps[c.Key] = make(map[string]bool)
	}

	for _, child := range children {
		links, linkErr := s.deps.Jira.FetchIssueLinks(ctx, child.Key)
		if linkErr != nil {
			continue
		}
		for _, link := range links {
			if link.Type != "Blocks" {
				continue
			}
			// "is blocked by" = inward, so if link.InwardKey is set and it's in our epic
			if link.InwardKey != "" && childKeys[link.InwardKey] {
				deps[child.Key][link.InwardKey] = true
			}
		}
	}

	// Topological sort into waves (Kahn's algorithm).
	childMap := make(map[string]jira.EpicChildIssue)
	for _, c := range children {
		childMap[c.Key] = c
	}

	assigned := make(map[string]int)
	waveNum := 1
	maxWave := 0
	for len(assigned) < len(children) {
		var ready []string
		for _, c := range children {
			if _, done := assigned[c.Key]; done {
				continue
			}
			allDepsResolved := true
			for dep := range deps[c.Key] {
				if _, done := assigned[dep]; !done {
					allDepsResolved = false
					break
				}
			}
			if allDepsResolved {
				ready = append(ready, c.Key)
			}
		}
		if len(ready) == 0 {
			// Circular dependency — assign remaining to current wave.
			for _, c := range children {
				if _, done := assigned[c.Key]; !done {
					assigned[c.Key] = waveNum
				}
			}
			break
		}
		for _, key := range ready {
			assigned[key] = waveNum
		}
		if waveNum > maxWave {
			maxWave = waveNum
		}
		waveNum++
	}

	// Build wave results.
	waveMap := make(map[int][]wavePlanTicket)
	var totalPoints float64
	maxPar := 0

	for _, c := range children {
		w := assigned[c.Key]
		repo := extractRepo(c.Labels, c.Summary)
		waveMap[w] = append(waveMap[w], wavePlanTicket{
			Key:     c.Key,
			Summary: c.Summary,
			Repo:    repo,
			Status:  c.Status,
			Points:  c.Points,
		})
		totalPoints += c.Points
	}

	var waves []wavePlanWave
	waveNums := make([]int, 0, len(waveMap))
	for w := range waveMap {
		waveNums = append(waveNums, w)
	}
	sort.Ints(waveNums)

	doneStatuses := map[string]bool{"Done": true, "Closed": true, "DONE": true, "Cancelled": true}

	for _, w := range waveNums {
		tickets := waveMap[w]
		if len(tickets) > maxPar {
			maxPar = len(tickets)
		}

		// Determine if wave is ready (all blockers in previous waves are DONE).
		ready := true
		var blockedBy []string
		for _, t := range tickets {
			for dep := range deps[t.Key] {
				depChild := childMap[dep]
				if !doneStatuses[depChild.Status] {
					ready = false
					blockedBy = append(blockedBy, dep)
				}
			}
		}

		waves = append(waves, wavePlanWave{
			Wave:      w,
			Tickets:   tickets,
			Ready:     ready,
			BlockedBy: unique(blockedBy),
		})
	}

	return wavePlanResult{
		Epic:        epic,
		Waves:       waves,
		TotalPoints: totalPoints,
		Parallelism: maxPar,
	}, nil
}

// --- Wave Launch ---

type waveLaunchArgs struct {
	Epic    string   `json:"epic"`
	Wave    *int     `json:"wave"`
	DAG     string   `json:"dag"`
	Tickets []string `json:"tickets"`
	DryRun  bool     `json:"dry_run"`
}

type launchedTicket struct {
	Ticket        string `json:"ticket"`
	CorrelationID string `json:"correlation_id"`
	Workspace     string `json:"workspace"`
}

type waveLaunchResult struct {
	Wave     int              `json:"wave"`
	Launched []launchedTicket `json:"launched"`
	Skipped  []string         `json:"skipped"`
	Errors   []string         `json:"errors"`
	DryRun   bool             `json:"dry_run"`
}

func (s *Server) toolWaveLaunch(ctx context.Context, raw json.RawMessage) (any, error) {
	var args waveLaunchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Epic == "" {
		return nil, fmt.Errorf("epic is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}
	if args.DAG == "" {
		args.DAG = "jira-dev"
	}

	// Get the wave plan.
	plan, err := s.computeWavePlan(ctx, args.Epic)
	if err != nil {
		return nil, fmt.Errorf("compute wave plan: %w", err)
	}

	// Find the target wave.
	targetWave := 0
	if args.Wave != nil {
		targetWave = *args.Wave
	} else {
		// Find first ready wave.
		for _, w := range plan.Waves {
			if w.Ready {
				targetWave = w.Wave
				break
			}
		}
	}
	if targetWave == 0 {
		return nil, fmt.Errorf("no ready wave found")
	}

	var waveTickets []wavePlanTicket
	for _, w := range plan.Waves {
		if w.Wave == targetWave {
			waveTickets = w.Tickets
			break
		}
	}

	// Filter to requested tickets if specified.
	if len(args.Tickets) > 0 {
		filter := make(map[string]bool)
		for _, t := range args.Tickets {
			filter[t] = true
		}
		var filtered []wavePlanTicket
		for _, t := range waveTickets {
			if filter[t.Key] {
				filtered = append(filtered, t)
			}
		}
		waveTickets = filtered
	}

	result := waveLaunchResult{
		Wave:   targetWave,
		DryRun: args.DryRun,
	}

	for _, ticket := range waveTickets {
		if args.DryRun {
			result.Launched = append(result.Launched, launchedTicket{
				Ticket:    ticket.Key,
				Workspace: fmt.Sprintf("$RICK_REPOS_PATH/%s-%s", ticket.Repo, ticket.Key),
			})
			continue
		}

		// Launch workflow via the existing rick_run_workflow handler.
		prompt := fmt.Sprintf("Implement Jira ticket %s: %s", ticket.Key, ticket.Summary)
		launchParams := map[string]any{
			"prompt": prompt,
			"dag":    args.DAG,
			"ticket": ticket.Key,
		}
		if ticket.Repo != "" {
			launchParams["repo"] = ticket.Repo
		}
		wfArgs, _ := json.Marshal(launchParams)

		wfResult, wfErr := s.toolRunWorkflow(ctx, wfArgs)
		if wfErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", ticket.Key, wfErr))
			continue
		}

		if wfr, ok := wfResult.(runWorkflowResult); ok {
			result.Launched = append(result.Launched, launchedTicket{
				Ticket:        ticket.Key,
				CorrelationID: wfr.CorrelationID,
			})
		}
	}

	return result, nil
}

// --- Wave Status ---

type waveStatusArgs struct {
	Epic string `json:"epic"`
	Wave *int   `json:"wave"`
}

type waveTicketStatus struct {
	Key            string `json:"key"`
	WorkflowStatus string `json:"workflow_status,omitempty"`
	Phase          string `json:"phase,omitempty"`
	JiraStatus     string `json:"jira_status,omitempty"`
}

type waveStatusResult struct {
	Epic      string             `json:"epic"`
	Wave      int                `json:"wave,omitempty"`
	Tickets   []waveTicketStatus `json:"tickets"`
	Completed int                `json:"completed"`
	Running   int                `json:"running"`
	Failed    int                `json:"failed"`
}

func (s *Server) toolWaveStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	var args waveStatusArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Epic == "" {
		return nil, fmt.Errorf("epic is required")
	}
	if err := s.requireJira(); err != nil {
		return nil, err
	}

	// Get wave plan to find ticket keys.
	plan, err := s.computeWavePlan(ctx, args.Epic)
	if err != nil {
		return nil, fmt.Errorf("compute wave plan: %w", err)
	}

	// Determine which wave(s) to show.
	var targetWaves []wavePlanWave
	if args.Wave != nil {
		for _, w := range plan.Waves {
			if w.Wave == *args.Wave {
				targetWaves = append(targetWaves, w)
				break
			}
		}
	} else {
		targetWaves = plan.Waves
	}

	result := waveStatusResult{Epic: args.Epic}
	if args.Wave != nil {
		result.Wave = *args.Wave
	}

	for _, wave := range targetWaves {
		for _, ticket := range wave.Tickets {
			ts := waveTicketStatus{
				Key:        ticket.Key,
				JiraStatus: ticket.Status,
			}

			// Look up workflow by tag.
			correlations, tagErr := s.deps.Store.LoadByTag(ctx, "ticket", ticket.Key)
			if tagErr == nil && len(correlations) > 0 {
				// Use the most recent correlation.
				corrID := correlations[len(correlations)-1]
				if ws := s.deps.Workflows; ws != nil {
					all := ws.All()
					for _, w := range all {
						if w.AggregateID == corrID {
							ts.WorkflowStatus = w.Status
							break
						}
					}
				}
			}

			switch ts.WorkflowStatus {
			case "completed":
				result.Completed++
			case "running":
				result.Running++
			case "failed":
				result.Failed++
			}

			result.Tickets = append(result.Tickets, ts)
		}
	}

	return result, nil
}

// --- Wave Cleanup ---

type waveCleanupArgs struct {
	Epic  string `json:"epic"`
	Wave  *int   `json:"wave"`
	Force bool   `json:"force"`
}

func (s *Server) toolWaveCleanup(ctx context.Context, raw json.RawMessage) (any, error) {
	var args waveCleanupArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.Epic == "" {
		return nil, fmt.Errorf("epic is required")
	}

	// Get the wave plan to identify ticket keys (also validates Jira is configured).
	plan, err := s.computeWavePlan(ctx, args.Epic)
	if err != nil {
		return nil, fmt.Errorf("compute wave plan: %w", err)
	}

	reposPath := os.Getenv("RICK_REPOS_PATH")
	if reposPath == "" {
		return nil, fmt.Errorf("RICK_REPOS_PATH environment variable is not set")
	}

	ticketKeys := make(map[string]bool)
	for _, wave := range plan.Waves {
		if args.Wave != nil && wave.Wave != *args.Wave {
			continue
		}
		for _, t := range wave.Tickets {
			ticketKeys[t.Key] = true
		}
	}

	// Scan $RICK_REPOS_PATH for matching workspace directories.
	entries, err := os.ReadDir(reposPath)
	if err != nil {
		return nil, fmt.Errorf("read RICK_REPOS_PATH: %w", err)
	}

	var cleaned []string
	var skipped []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		for key := range ticketKeys {
			if strings.Contains(name, key) {
				path := filepath.Join(reposPath, name)
				// Reuse the same safety guard as workspace cleanup.
				if _, safeErr := safeWorkspacePath(path); safeErr != nil {
					skipped = append(skipped, fmt.Sprintf("%s: safety check failed: %v", name, safeErr))
					break
				}
				if err := os.RemoveAll(path); err != nil {
					skipped = append(skipped, fmt.Sprintf("%s: %v", name, err))
				} else {
					cleaned = append(cleaned, name)
				}
				break
			}
		}
	}

	return map[string]any{
		"epic":    args.Epic,
		"cleaned": cleaned,
		"skipped": skipped,
	}, nil
}

// --- Helpers ---

func extractRepo(labels []string, summary string) string {
	for _, label := range labels {
		if strings.HasPrefix(label, "repo:") {
			return strings.TrimPrefix(label, "repo:")
		}
	}
	// Try to extract repo from summary prefix like "backend: ..."
	if idx := strings.Index(summary, ":"); idx > 0 && idx < 30 {
		return strings.TrimSpace(summary[:idx])
	}
	return ""
}

func unique(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	result := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
