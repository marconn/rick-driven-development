package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	gh "github.com/marconn/rick-event-driven-development/internal/github"
	"github.com/marconn/rick-event-driven-development/internal/jira"
)

func (s *Server) registerWaveTools() {

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_plan",
			Description: "Compute development waves from a parent work item. Supports a Jira epic (source.type=jira) or a GitHub parent issue with sub-issues (source.type=github). Reads children and dependency links, performs topological sort, returns a wave schedule showing which items can be developed in parallel. Back-compat: passing 'epic' alone is equivalent to {source:{type:'jira',epic:<value>}}.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic": map[string]any{
						"type":        "string",
						"description": "Jira epic key (back-compat shorthand for source.type='jira').",
					},
					"source": map[string]any{
						"type":        "object",
						"description": "Structured source descriptor. Use this for GitHub waves.",
						"properties": map[string]any{
							"type": map[string]any{
								"type": "string",
								"enum": []string{"jira", "github"},
							},
							"epic": map[string]any{
								"type":        "string",
								"description": "Jira epic key (when type='jira').",
							},
							"parent": map[string]any{
								"type":        "string",
								"description": "GitHub parent issue as 'owner/repo#N' (when type='github').",
							},
							"child_discovery": map[string]any{
								"type":        "string",
								"enum":        []string{"sub_issues", "task_list", "body_refs", "auto"},
								"default":     "sub_issues",
								"description": "How to discover children. 'auto' tries sub_issues → task_list → body_refs and records the winner in diagnostics.discovery_path.",
							},
							"dependency_source": map[string]any{
								"type":        "string",
								"enum":        []string{"table", "body_refs", "labels", "none"},
								"default":     "table",
								"description": "Dependency source: 'table' (Depends-on column in parent body), 'body_refs' (scan each child body for Depends-on / Blocks / Blocked-by keywords), 'labels' (depends:<number>), or 'none'.",
							},
							"dag_options": map[string]any{
								"type":        "object",
								"description": "Per-child DAG selection rules. Omit to keep default routing.",
								"properties": map[string]any{
									"dag_map": map[string]any{
										"type":                 "object",
										"description":          "Label → DAG mapping. Use 'default' key for fallback. rick:* labels absent from the map are reported in diagnostics.skipped as 'unknown_dag_label'.",
										"additionalProperties": map[string]any{"type": "string"},
									},
								},
							},
						},
					},
				},
			},
		},
		Handler: s.toolWavePlan,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_launch",
			Description: "Launch a wave of parallel development jobs. Accepts the same source shape as rick_wave_plan (Jira epic or GitHub parent). For each child: starts the configured workflow with the appropriate source tag for later status lookup.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic":   map[string]any{"type": "string", "description": "Jira epic key (back-compat shorthand)."},
					"source": waveSourceSchema(),
					"wave": map[string]any{
						"type":        "integer",
						"description": "Wave number to launch (from rick_wave_plan). Omit for next ready wave.",
					},
					"dag": map[string]any{
						"type":        "string",
						"description": "Workflow DAG to use for each child. Default: 'jira-dev' for Jira sources, 'workspace-dev' for GitHub sources.",
					},
					"tickets": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Override: specific child IDs to launch (subset of wave). For Jira: 'PROJ-123'. For GitHub: 'owner/repo#N'.",
					},
					"dry_run": map[string]any{"type": "boolean", "default": false},
				},
			},
		},
		Handler: s.toolWaveLaunch,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_status",
			Description: "Monitor the progress of a launched wave. Shows workflow status for each child plus aggregate view. Supports Jira and GitHub sources.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic":   map[string]any{"type": "string", "description": "Jira epic key (back-compat shorthand)."},
					"source": waveSourceSchema(),
					"wave":   map[string]any{"type": "integer", "description": "Wave number (optional, shows all waves if omitted)."},
				},
			},
		},
		Handler: s.toolWaveStatus,
	})

	s.register(Tool{
		Definition: ToolDefinition{
			Name:        "rick_wave_cleanup",
			Description: "Remove all isolated workspaces for a wave. Supports Jira and GitHub sources.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"epic":   map[string]any{"type": "string", "description": "Jira epic key (back-compat shorthand)."},
					"source": waveSourceSchema(),
					"wave":   map[string]any{"type": "integer", "description": "Wave number to clean up."},
					"force":  map[string]any{"type": "boolean", "default": false},
				},
			},
		},
		Handler: s.toolWaveCleanup,
	})
}

// waveSourceSchema is the shared JSON schema fragment for the structured
// source descriptor accepted by all wave tools.
func waveSourceSchema() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": "Structured source descriptor. Use this for GitHub waves.",
		"properties": map[string]any{
			"type":              map[string]any{"type": "string", "enum": []string{"jira", "github"}},
			"epic":              map[string]any{"type": "string"},
			"parent":            map[string]any{"type": "string"},
			"child_discovery":   map[string]any{"type": "string", "enum": []string{"sub_issues", "task_list", "body_refs", "auto"}},
			"dependency_source": map[string]any{"type": "string", "enum": []string{"table", "body_refs", "labels", "none"}},
			"dag_options":       map[string]any{"type": "object"},
		},
	}
}

// --- Wave computation ---

// wavePlanTicket is the per-node shape of a wave plan. Fields are shared by
// both Jira and GitHub sources; some only populate one side (e.g. Points is
// Jira-only, Body is GitHub-only).
type wavePlanTicket struct {
	// Key is retained for back-compat with existing callers — it equals
	// ID for both Jira (same value) and GitHub sources (owner/repo#N).
	Key       string         `json:"key"`
	ID        string         `json:"id"`
	IDKind    string         `json:"id_kind"` // "jira" | "github"
	Summary   string         `json:"summary"`
	Body      string         `json:"body,omitempty"`
	Repo      string         `json:"repo,omitempty"`
	Status    string         `json:"status"`
	Points    float64        `json:"points,omitempty"`
	DAGParams map[string]any `json:"dag_params,omitempty"`
}

type wavePlanWave struct {
	Wave      int              `json:"wave"`
	Tickets   []wavePlanTicket `json:"tickets"`
	Ready     bool             `json:"ready"`
	BlockedBy []string         `json:"blocked_by,omitempty"`
}

type wavePlanSource struct {
	Type   string `json:"type"`
	Epic   string `json:"epic,omitempty"`
	Parent string `json:"parent,omitempty"`
}

type wavePlanDiagnostics struct {
	DiscoveryPath  string              `json:"discovery_path,omitempty"`
	DependencyPath string              `json:"dependency_path,omitempty"`
	Cycles         [][]string          `json:"cycles,omitempty"`
	Orphans        []string            `json:"orphans,omitempty"`
	Skipped        []wavePlanSkipEntry `json:"skipped,omitempty"`
	Reason         string              `json:"reason,omitempty"`
}

type wavePlanSkipEntry struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type wavePlanResult struct {
	Epic        string              `json:"epic,omitempty"`
	Source      wavePlanSource      `json:"source"`
	Waves       []wavePlanWave      `json:"waves"`
	TotalPoints float64             `json:"total_points,omitempty"`
	Parallelism int                 `json:"parallelism"`
	Diagnostics wavePlanDiagnostics `json:"diagnostics"`
}

// wavePlanArgs is the parsed form of a rick_wave_plan call. We accept both the
// legacy `epic` shorthand and the structured `source` discriminator.
type wavePlanArgs struct {
	Epic   string           `json:"epic"`
	Source *wavePlanSourceIn `json:"source"`
}

type wavePlanSourceIn struct {
	Type             string             `json:"type"`
	Epic             string             `json:"epic"`
	Parent           string             `json:"parent"`
	ChildDiscovery   string             `json:"child_discovery"`
	DependencySource string             `json:"dependency_source"`
	DAGOptions       *dagOptionsIn      `json:"dag_options,omitempty"`
}

// dagOptionsIn carries per-workflow DAG-selection rules. Phase 2: dag_map is
// a label → workflow-name map with a `default` key. Any label starting with
// `rick:` that is absent from dag_map is considered a typo and recorded in
// diagnostics.skipped with reason `unknown_dag_label`.
type dagOptionsIn struct {
	DAGMap map[string]string `json:"dag_map,omitempty"`
}

// resolvedWaveSource is the normalized form used by wave_launch/status/cleanup
// after parsing args. Exactly one of Epic or GHParent is populated.
type resolvedWaveSource struct {
	Kind             string // "jira" | "github"
	Epic             string
	GHOwner, GHRepo  string
	GHNumber         int
	ChildDiscovery   string
	DependencySource string
	DAGMap           map[string]string
}

func (r resolvedWaveSource) ghParent() string {
	return fmt.Sprintf("%s/%s#%d", r.GHOwner, r.GHRepo, r.GHNumber)
}

func parseWaveSource(args wavePlanArgs) (resolvedWaveSource, error) {
	// Back-compat: bare `epic` → Jira source.
	if args.Source == nil {
		if args.Epic == "" {
			return resolvedWaveSource{}, fmt.Errorf("either 'epic' or 'source' is required")
		}
		return resolvedWaveSource{Kind: "jira", Epic: args.Epic}, nil
	}
	src := args.Source
	switch src.Type {
	case "jira":
		epic := src.Epic
		if epic == "" {
			epic = args.Epic
		}
		if epic == "" {
			return resolvedWaveSource{}, fmt.Errorf("source.epic is required when source.type='jira'")
		}
		return resolvedWaveSource{Kind: "jira", Epic: epic}, nil
	case "github":
		if src.Parent == "" {
			return resolvedWaveSource{}, fmt.Errorf("source.parent is required when source.type='github'")
		}
		owner, repo, num, err := parseGHParent(src.Parent)
		if err != nil {
			return resolvedWaveSource{}, err
		}
		discovery := src.ChildDiscovery
		if discovery == "" {
			discovery = "sub_issues"
		}
		switch discovery {
		case "sub_issues", "task_list", "body_refs", "auto":
		default:
			return resolvedWaveSource{}, fmt.Errorf("source.child_discovery=%q unsupported (use 'sub_issues', 'task_list', 'body_refs', or 'auto')", discovery)
		}
		depSrc := src.DependencySource
		if depSrc == "" {
			depSrc = "table"
		}
		switch depSrc {
		case "table", "body_refs", "labels", "none":
		default:
			return resolvedWaveSource{}, fmt.Errorf("source.dependency_source=%q unsupported (use 'table', 'body_refs', 'labels', or 'none')", depSrc)
		}
		var dagMap map[string]string
		if src.DAGOptions != nil {
			dagMap = src.DAGOptions.DAGMap
		}
		return resolvedWaveSource{
			Kind:             "github",
			GHOwner:          owner,
			GHRepo:           repo,
			GHNumber:         num,
			ChildDiscovery:   discovery,
			DependencySource: depSrc,
			DAGMap:           dagMap,
		}, nil
	default:
		return resolvedWaveSource{}, fmt.Errorf("unknown source.type=%q (expected 'jira' or 'github')", src.Type)
	}
}

// parseGHParent accepts "owner/repo#N". Rejects anything else with a clear
// error so callers don't silently end up with zero children.
func parseGHParent(s string) (string, string, int, error) {
	slash := strings.Index(s, "/")
	hash := strings.Index(s, "#")
	if slash <= 0 || hash <= slash+1 || hash == len(s)-1 {
		return "", "", 0, fmt.Errorf("invalid github parent %q (expected 'owner/repo#N')", s)
	}
	owner := s[:slash]
	repo := s[slash+1 : hash]
	num, err := strconv.Atoi(s[hash+1:])
	if err != nil || num <= 0 {
		return "", "", 0, fmt.Errorf("invalid github parent %q: issue number must be positive integer", s)
	}
	return owner, repo, num, nil
}

func (s *Server) toolWavePlan(ctx context.Context, raw json.RawMessage) (any, error) {
	var args wavePlanArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	src, err := parseWaveSource(args)
	if err != nil {
		return nil, err
	}
	return s.computeWavePlanForSource(ctx, src)
}

// computeWavePlan is a back-compat shim for Jira-epic callers — predates the
// source discriminator. New code should use computeWavePlanForSource.
func (s *Server) computeWavePlan(ctx context.Context, epic string) (wavePlanResult, error) {
	return s.computeJiraWavePlan(ctx, epic)
}

// computeWavePlanForSource routes to the Jira or GitHub implementation.
func (s *Server) computeWavePlanForSource(ctx context.Context, src resolvedWaveSource) (wavePlanResult, error) {
	switch src.Kind {
	case "jira":
		return s.computeJiraWavePlan(ctx, src.Epic)
	case "github":
		return s.computeGithubWavePlan(ctx, src)
	default:
		return wavePlanResult{}, fmt.Errorf("unsupported source kind %q", src.Kind)
	}
}

// computeJiraWavePlan is the legacy Jira implementation; kept intact, wrapped
// in the shared wavePlanResult shape with id_kind="jira".
func (s *Server) computeJiraWavePlan(ctx context.Context, epic string) (wavePlanResult, error) {
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
			ID:      c.Key,
			IDKind:  "jira",
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
		Source:      wavePlanSource{Type: "jira", Epic: epic},
		Waves:       waves,
		TotalPoints: totalPoints,
		Parallelism: maxPar,
		Diagnostics: wavePlanDiagnostics{
			DiscoveryPath:  "jira_epic_children",
			DependencyPath: "jira_links_blocks",
		},
	}, nil
}

// computeGithubWavePlan orchestrates the GitHub wave-planning pipeline.
// Structured to keep discovery, dependency derivation, and DAG selection
// independently swappable per spec §4/§5.1.
func (s *Server) computeGithubWavePlan(ctx context.Context, src resolvedWaveSource) (wavePlanResult, error) {
	if s.deps.GitHub == nil {
		return wavePlanResult{}, fmt.Errorf("GITHUB_TOKEN is not set — rick_wave_plan with source.type='github' requires a configured GitHub client")
	}

	// Opt-in GraphQL fast path (spec §8). Collapses parent + sub-issues +
	// timeline into one round-trip when RICK_GITHUB_GRAPHQL=1 and the source
	// uses sub_issues discovery. Falls back to the REST pipeline on any
	// error so we never leave the user stuck if the fast path misfires.
	if os.Getenv("RICK_GITHUB_GRAPHQL") != "" && src.ChildDiscovery == "sub_issues" {
		if plan, ok := s.tryGraphQLWavePlan(ctx, src); ok {
			return plan, nil
		}
	}

	parent, err := s.deps.GitHub.GetIssue(ctx, src.GHOwner, src.GHRepo, src.GHNumber)
	if err != nil {
		return wavePlanResult{}, fmt.Errorf("fetch parent issue: %w", err)
	}
	if parent.PullRequest != nil {
		return wavePlanResult{}, fmt.Errorf("parent %s/%s#%d is a pull request, not an issue", src.GHOwner, src.GHRepo, src.GHNumber)
	}
	if parent.State == "closed" {
		return wavePlanResult{}, fmt.Errorf("parent issue %s/%s#%d is closed — cannot plan a wave on a closed epic", src.GHOwner, src.GHRepo, src.GHNumber)
	}

	parentRepo := src.GHOwner + "/" + src.GHRepo
	result := wavePlanResult{
		Source: wavePlanSource{Type: "github", Parent: src.ghParent()},
	}

	nodes, order, discoveryPath, err := s.discoverGithubChildren(ctx, src, parent, parentRepo, &result)
	if err != nil {
		return wavePlanResult{}, err
	}
	result.Diagnostics.DiscoveryPath = discoveryPath
	if len(nodes) == 0 {
		if result.Diagnostics.Reason == "" {
			result.Diagnostics.Reason = "no_children_discovered"
		}
		return result, nil
	}

	preds, depPath, err := s.buildGithubDependencies(ctx, src, parent, parentRepo, nodes, &result)
	if err != nil {
		return wavePlanResult{}, err
	}
	result.Diagnostics.DependencyPath = depPath

	waves, maxPar, err := kahnWaves(order, preds, &result)
	if err != nil {
		return wavePlanResult{}, err
	}

	// DAG selection per child runs before we materialize tickets so that
	// ticket.dag_params carries the resolved workflow / PR source.
	s.assignDAGParams(ctx, src, nodes, parentRepo, &result)

	result.Waves = buildWaveTickets(waves, nodes, order)
	result.Parallelism = maxPar

	// Orphans: nodes with no preds and no dependents — flag informationally.
	dependents := make(map[string]int)
	for _, ps := range preds {
		for p := range ps {
			dependents[p]++
		}
	}
	for id := range nodes {
		if len(preds[id]) == 0 && dependents[id] == 0 {
			result.Diagnostics.Orphans = append(result.Diagnostics.Orphans, id)
		}
	}
	sort.Strings(result.Diagnostics.Orphans)
	return result, nil
}

// waveNode is the per-child working set carried through discovery, dep
// building, and DAG selection.
type waveNode struct {
	id     string
	repo   string
	number int
	title  string
	body   string
	state  string
	labels []string
	dag    string
	prRef  string // populated when PR-feedback routing picked a PR.
}

// discoverGithubChildren resolves the set of children using the configured
// discovery mode. Returns the node map, iteration order, and the path used
// (so the caller can record it in diagnostics.discovery_path).
func (s *Server) discoverGithubChildren(ctx context.Context, src resolvedWaveSource, parent *gh.Issue, parentRepo string, result *wavePlanResult) (map[string]*waveNode, []string, string, error) {
	mode := src.ChildDiscovery
	tryOrder := []string{mode}
	if mode == "auto" {
		tryOrder = []string{"sub_issues", "task_list", "body_refs"}
	}

	var lastErr error
	for _, attempt := range tryOrder {
		nodes, order, attemptErr := s.discoverOnce(ctx, attempt, src, parent, parentRepo, result)
		if attemptErr != nil {
			lastErr = attemptErr
			continue
		}
		if len(nodes) > 0 {
			return nodes, order, attempt, nil
		}
	}
	if lastErr != nil {
		return nil, nil, "", lastErr
	}
	// No attempt yielded children — return the first mode name so callers can
	// still read a sensible discovery_path in diagnostics.
	return map[string]*waveNode{}, nil, tryOrder[0], nil
}

// discoverOnce runs a single discovery mode. `sub_issues` uses the dedicated
// endpoint; the two body-based modes fall through to per-ref issue fetches.
func (s *Server) discoverOnce(ctx context.Context, mode string, src resolvedWaveSource, parent *gh.Issue, parentRepo string, result *wavePlanResult) (map[string]*waveNode, []string, error) {
	switch mode {
	case "sub_issues":
		subs, err := s.deps.GitHub.GetSubIssues(ctx, src.GHOwner, src.GHRepo, src.GHNumber)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch sub-issues: %w", err)
		}
		nodes := s.materializeFromSubIssues(subs, parentRepo, result)
		order := make([]string, 0, len(subs))
		for _, sub := range subs {
			repo := parentRepo
			if sub.Repository != nil && sub.Repository.FullName != "" {
				repo = sub.Repository.FullName
			}
			id := fmt.Sprintf("%s#%d", repo, sub.Number)
			if _, ok := nodes[id]; ok {
				order = append(order, id)
			}
		}
		return nodes, order, nil
	case "task_list":
		refs := gh.ParseTaskList(parent.Body)
		return s.materializeFromRefs(ctx, refs, parentRepo, result)
	case "body_refs":
		refs := gh.ParseBodyRefs(parent.Body)
		// Parent itself shouldn't be its own child.
		refs = filterSelf(refs, gh.IssueRef{Owner: src.GHOwner, Repo: src.GHRepo, Number: src.GHNumber}, parentRepo)
		return s.materializeFromRefs(ctx, refs, parentRepo, result)
	default:
		return nil, nil, fmt.Errorf("unsupported discovery mode %q", mode)
	}
}

// materializeFromSubIssues converts SubIssue payloads into waveNodes and
// records filter reasons in diagnostics.skipped. Order is preserved.
func (s *Server) materializeFromSubIssues(subs []gh.SubIssue, parentRepo string, result *wavePlanResult) map[string]*waveNode {
	nodes := make(map[string]*waveNode, len(subs))
	for _, sub := range subs {
		repo := parentRepo
		if sub.Repository != nil && sub.Repository.FullName != "" {
			repo = sub.Repository.FullName
		}
		id := fmt.Sprintf("%s#%d", repo, sub.Number)

		if sub.State == "closed" {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: "closed"})
			continue
		}
		if sub.PullRequest != nil {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: "is_pull_request"})
			continue
		}
		if repo != parentRepo {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: "cross_repo_not_supported"})
			continue
		}
		labels := make([]string, 0, len(sub.Labels))
		for _, l := range sub.Labels {
			labels = append(labels, l.Name)
		}
		nodes[id] = &waveNode{id: id, repo: repo, number: sub.Number, title: sub.Title, body: sub.Body, state: sub.State, labels: labels}
	}
	return nodes
}

// materializeFromRefs fetches each referenced issue to populate title/state/
// labels/body — needed when body-based discovery yields bare numbers. Returns
// the node map plus preserved-order slice; failures on individual issues are
// non-fatal (recorded in diagnostics.skipped).
func (s *Server) materializeFromRefs(ctx context.Context, refs []gh.IssueRef, parentRepo string, result *wavePlanResult) (map[string]*waveNode, []string, error) {
	nodes := make(map[string]*waveNode)
	order := make([]string, 0, len(refs))
	for _, ref := range refs {
		repo := parentRepo
		if ref.Owner != "" && ref.Repo != "" {
			repo = ref.Owner + "/" + ref.Repo
		}
		id := fmt.Sprintf("%s#%d", repo, ref.Number)
		if _, dup := nodes[id]; dup {
			continue
		}
		if repo != parentRepo {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: "cross_repo_not_supported"})
			continue
		}
		owner, name := splitRepoFullName(repo)
		issue, err := s.deps.GitHub.GetIssue(ctx, owner, name, ref.Number)
		if err != nil {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: fmt.Sprintf("fetch failed: %v", err)})
			continue
		}
		if issue.PullRequest != nil {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: "is_pull_request"})
			continue
		}
		if issue.State == "closed" {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: "closed"})
			continue
		}
		labels := make([]string, 0, len(issue.Labels))
		for _, l := range issue.Labels {
			labels = append(labels, l.Name)
		}
		nodes[id] = &waveNode{id: id, repo: repo, number: issue.Number, title: issue.Title, body: issue.Body, state: issue.State, labels: labels}
		order = append(order, id)
	}
	return nodes, order, nil
}

// buildGithubDependencies produces the predecessor map per the configured
// dependency source. Unknown refs are recorded as skipped rather than failing.
func (s *Server) buildGithubDependencies(ctx context.Context, src resolvedWaveSource, parent *gh.Issue, parentRepo string, nodes map[string]*waveNode, result *wavePlanResult) (map[string]map[string]bool, string, error) {
	preds := make(map[string]map[string]bool, len(nodes))
	for id := range nodes {
		preds[id] = make(map[string]bool)
	}

	addEdge := func(fromID, onID string, ctxLabel string) {
		if _, ok := nodes[fromID]; !ok {
			return
		}
		if _, ok := nodes[onID]; !ok {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{
				ID:     onID,
				Reason: fmt.Sprintf("%s dep of %s references unknown issue — treated as satisfied", ctxLabel, fromID),
			})
			return
		}
		preds[fromID][onID] = true
	}

	switch src.DependencySource {
	case "table":
		for _, edge := range gh.ParseDependencyTable(parent.Body) {
			addEdge(canonicalizeEdgeRef(edge.From, parentRepo), canonicalizeEdgeRef(edge.On, parentRepo), "table")
		}
		return preds, "table", nil
	case "body_refs":
		for _, node := range nodes {
			body := node.body
			if body == "" {
				owner, name := splitRepoFullName(node.repo)
				issue, err := s.deps.GitHub.GetIssue(ctx, owner, name, node.number)
				if err != nil {
					result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: node.id, Reason: fmt.Sprintf("fetch body for body_refs: %v", err)})
					continue
				}
				body = issue.Body
				node.body = body
			}
			self := gh.IssueRef{Number: node.number}
			if o, r := splitRepoFullName(node.repo); o != "" {
				self.Owner = o
				self.Repo = r
			}
			for _, edge := range gh.ParseBodyDependencies(self, body) {
				addEdge(canonicalizeEdgeRef(edge.From, parentRepo), canonicalizeEdgeRef(edge.On, parentRepo), "body_refs")
			}
		}
		return preds, "body_refs", nil
	case "labels":
		for _, node := range nodes {
			for _, lab := range node.labels {
				dep, ok := parseDependsLabel(lab)
				if !ok {
					continue
				}
				onID := canonicalizeEdgeRef(dep, parentRepo)
				addEdge(node.id, onID, "labels")
			}
		}
		return preds, "labels", nil
	case "none":
		return preds, "none", nil
	default:
		return nil, "", fmt.Errorf("unsupported dependency source %q", src.DependencySource)
	}
}

// kahnWaves runs a Kahn topological sort that respects the insertion order of
// `order` for stable output. Returns a per-node wave index, the maximum
// parallelism, and an error for cycles (recorded into diagnostics).
func kahnWaves(order []string, preds map[string]map[string]bool, result *wavePlanResult) (map[string]int, int, error) {
	assigned := make(map[string]int, len(preds))
	waveNum := 1
	maxPar := 0
	// If order wasn't supplied (e.g. sub_issues path), derive it.
	if len(order) == 0 {
		order = make([]string, 0, len(preds))
		for id := range preds {
			order = append(order, id)
		}
		sort.Strings(order)
	}
	for len(assigned) < len(preds) {
		var ready []string
		for _, id := range order {
			if _, done := assigned[id]; done {
				continue
			}
			if _, ok := preds[id]; !ok {
				continue
			}
			all := true
			for p := range preds[id] {
				if _, done := assigned[p]; !done {
					all = false
					break
				}
			}
			if all {
				ready = append(ready, id)
			}
		}
		if len(ready) == 0 {
			var remaining []string
			for _, id := range order {
				if _, done := assigned[id]; !done {
					remaining = append(remaining, id)
				}
			}
			result.Diagnostics.Cycles = append(result.Diagnostics.Cycles, remaining)
			return nil, 0, fmt.Errorf("cycle detected in dependency graph: %v", remaining)
		}
		for _, id := range ready {
			assigned[id] = waveNum
		}
		if len(ready) > maxPar {
			maxPar = len(ready)
		}
		waveNum++
	}
	return assigned, maxPar, nil
}

// buildWaveTickets groups nodes per wave preserving the discovery order.
// DAGParams are populated later by assignDAGParams; we record Body here so
// the launcher can include it in its prompt without a second fetch.
func buildWaveTickets(assigned map[string]int, nodes map[string]*waveNode, order []string) []wavePlanWave {
	if len(order) == 0 {
		for id := range nodes {
			order = append(order, id)
		}
		sort.Strings(order)
	}
	waveMap := make(map[int][]wavePlanTicket)
	for _, id := range order {
		n, ok := nodes[id]
		if !ok {
			continue
		}
		w := assigned[id]
		ticket := wavePlanTicket{
			Key:     n.id,
			ID:      n.id,
			IDKind:  "github",
			Summary: n.title,
			Body:    n.body,
			Repo:    n.repo,
			Status:  n.state,
		}
		if n.dag != "" || n.prRef != "" {
			ticket.DAGParams = map[string]any{}
			if n.dag != "" {
				ticket.DAGParams["dag"] = n.dag
			}
			if n.prRef != "" {
				ticket.DAGParams["pr_source"] = n.prRef
			}
		}
		waveMap[w] = append(waveMap[w], ticket)
	}
	nums := make([]int, 0, len(waveMap))
	for w := range waveMap {
		nums = append(nums, w)
	}
	sort.Ints(nums)
	waves := make([]wavePlanWave, 0, len(nums))
	for _, w := range nums {
		waves = append(waves, wavePlanWave{Wave: w, Tickets: waveMap[w], Ready: true})
	}
	return waves
}

// assignDAGParams chooses the per-child DAG using the rules from spec §5.1:
//  1. rick:<name> label matching dag_map → use that DAG.
//  2. rick:* label with no mapping → skipped (diagnostics).
//  3. Otherwise, if the child has an open PR referencing it (Closes/Fixes) →
//     pr-feedback against that PR.
//  4. Fall back to dag_map["default"] or "workspace-dev".
//
// The chosen DAG is written back into waveNode.dag so the launcher can pick
// it up without re-running label logic.
func (s *Server) assignDAGParams(ctx context.Context, src resolvedWaveSource, nodes map[string]*waveNode, parentRepo string, result *wavePlanResult) {
	dagMap := src.DAGMap
	defaultDAG := "workspace-dev"
	if v, ok := dagMap["default"]; ok && v != "" {
		defaultDAG = v
	}

	for id, node := range nodes {
		dag, prRef, reason := s.pickDAG(ctx, node, dagMap, defaultDAG, parentRepo)
		if reason != "" {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: reason})
		}
		node.dag = dag
		node.prRef = prRef
	}
}

// pickDAG is the pure decision function for a single child. Returns the
// chosen DAG, an optional PR reference (for pr-feedback), and a diagnostic
// reason string (non-empty when we want to record something).
func (s *Server) pickDAG(ctx context.Context, node *waveNode, dagMap map[string]string, defaultDAG, parentRepo string) (string, string, string) {
	// 1. rick:* labels.
	for _, lab := range node.labels {
		key, isRick := strings.CutPrefix(lab, "rick:")
		if !isRick {
			continue
		}
		if dag, ok := dagMap[lab]; ok && dag != "" {
			return dag, "", ""
		}
		if dag, ok := dagMap["rick:"+key]; ok && dag != "" {
			return dag, "", ""
		}
		// Known convention even without dag_map entry.
		switch key {
		case "develop-only", "ci-fix", "pr-feedback":
			return key, "", ""
		}
		// rick:* label not routed anywhere — typo signal.
		return defaultDAG, "", fmt.Sprintf("unknown rick label %q — falling back to %s", lab, defaultDAG)
	}

	// 2. PR linkage. GraphQL fast path pre-populates node.prRef; otherwise
	// consult the REST timeline endpoint on demand.
	if node.prRef != "" {
		return "pr-feedback", node.prRef, ""
	}
	if s.deps.GitHub != nil {
		owner, repo := splitRepoFullName(node.repo)
		events, err := s.deps.GitHub.GetIssueTimeline(ctx, owner, repo, node.number)
		if err == nil {
			for _, ev := range events {
				if ev.Event != "cross-referenced" || ev.Source == nil || ev.Source.Issue == nil {
					continue
				}
				src := ev.Source.Issue
				if src.PullRequest == nil {
					continue
				}
				if src.State != "open" {
					continue
				}
				ref := fmt.Sprintf("gh:%s#%d", parentRepo, src.Number)
				return "pr-feedback", ref, ""
			}
		}
	}

	// 3. Fallback.
	return defaultDAG, "", ""
}

// filterSelf removes any IssueRef that refers to `self`.
func filterSelf(refs []gh.IssueRef, self gh.IssueRef, parentRepo string) []gh.IssueRef {
	out := make([]gh.IssueRef, 0, len(refs))
	selfOwner, selfRepo := splitRepoFullName(parentRepo)
	if self.Owner == "" {
		self.Owner = selfOwner
	}
	if self.Repo == "" {
		self.Repo = selfRepo
	}
	for _, r := range refs {
		owner := r.Owner
		if owner == "" {
			owner = selfOwner
		}
		repo := r.Repo
		if repo == "" {
			repo = selfRepo
		}
		if r.Number == self.Number && owner == self.Owner && repo == self.Repo {
			continue
		}
		out = append(out, r)
	}
	return out
}

// parseDependsLabel decodes `depends:<N>` or `depends:<owner>-<repo>-<N>`
// label conventions into an IssueRef. Returns ok=false when the label is not
// a dependency label.
func parseDependsLabel(label string) (gh.IssueRef, bool) {
	rest, ok := strings.CutPrefix(label, "depends:")
	if !ok {
		return gh.IssueRef{}, false
	}
	// Form 1: bare number.
	if n, err := strconv.Atoi(rest); err == nil && n > 0 {
		return gh.IssueRef{Number: n}, true
	}
	// Form 2: owner-repo-<N> (labels can't contain '/').
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 {
		return gh.IssueRef{}, false
	}
	n, err := strconv.Atoi(rest[idx+1:])
	if err != nil || n <= 0 {
		return gh.IssueRef{}, false
	}
	ownerRepo := rest[:idx]
	mid := strings.Index(ownerRepo, "-")
	if mid <= 0 || mid == len(ownerRepo)-1 {
		return gh.IssueRef{}, false
	}
	return gh.IssueRef{Owner: ownerRepo[:mid], Repo: ownerRepo[mid+1:], Number: n}, true
}

// splitRepoFullName splits "owner/repo" into its parts; empty values on
// malformed input.
func splitRepoFullName(full string) (string, string) {
	i := strings.Index(full, "/")
	if i <= 0 || i == len(full)-1 {
		return "", ""
	}
	return full[:i], full[i+1:]
}

// canonicalizeEdgeRef converts a dep-table IssueRef into a canonical
// "owner/repo#N" ID, filling in the parent repo when the row omitted it.
func canonicalizeEdgeRef(r gh.IssueRef, parentRepo string) string {
	if r.Owner != "" && r.Repo != "" {
		return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number)
	}
	return fmt.Sprintf("%s#%d", parentRepo, r.Number)
}

// tryGraphQLWavePlan runs the single-query fast path. Returns (plan, true)
// on success, or (_, false) to signal fallback to REST. Errors are swallowed
// on purpose — this path is opt-in and non-authoritative.
func (s *Server) tryGraphQLWavePlan(ctx context.Context, src resolvedWaveSource) (wavePlanResult, bool) {
	resp, err := s.deps.GitHub.FetchWaveParent(ctx, src.GHOwner, src.GHRepo, src.GHNumber)
	if err != nil {
		return wavePlanResult{}, false
	}
	issue := resp.Repository.Issue
	if issue.Number == 0 {
		return wavePlanResult{}, false
	}
	if issue.State == "closed" {
		return wavePlanResult{}, false
	}

	parentRepo := src.GHOwner + "/" + src.GHRepo
	result := wavePlanResult{
		Source: wavePlanSource{Type: "github", Parent: src.ghParent()},
		Diagnostics: wavePlanDiagnostics{
			DiscoveryPath: "sub_issues_graphql",
		},
	}

	nodes := make(map[string]*waveNode, len(issue.SubIssues.Nodes))
	order := make([]string, 0, len(issue.SubIssues.Nodes))
	for _, sub := range issue.SubIssues.Nodes {
		repo := sub.Repository.NameWithOwner
		if repo == "" {
			repo = parentRepo
		}
		id := fmt.Sprintf("%s#%d", repo, sub.Number)
		if sub.State == "closed" {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: "closed"})
			continue
		}
		if repo != parentRepo {
			result.Diagnostics.Skipped = append(result.Diagnostics.Skipped, wavePlanSkipEntry{ID: id, Reason: "cross_repo_not_supported"})
			continue
		}
		labels := make([]string, 0, len(sub.Labels.Nodes))
		for _, l := range sub.Labels.Nodes {
			labels = append(labels, l.Name)
		}
		n := &waveNode{
			id: id, repo: repo, number: sub.Number,
			title: sub.Title, body: sub.Body, state: sub.State, labels: labels,
		}
		// Precompute PR linkage from timeline so we skip the per-child REST call.
		for _, tl := range sub.TimelineItems.Nodes {
			if tl.Source.Typename != "PullRequest" || tl.Source.State != "open" {
				continue
			}
			prRepo := tl.Source.Repository.NameWithOwner
			if prRepo == "" {
				prRepo = parentRepo
			}
			n.prRef = fmt.Sprintf("gh:%s#%d", prRepo, tl.Source.Number)
			break
		}
		nodes[id] = n
		order = append(order, id)
	}

	if len(nodes) == 0 {
		result.Diagnostics.Reason = "no_children_discovered"
		return result, true
	}

	preds, depPath, err := s.buildGithubDependencies(ctx, src, &gh.Issue{Body: issue.Body}, parentRepo, nodes, &result)
	if err != nil {
		return wavePlanResult{}, false
	}
	result.Diagnostics.DependencyPath = depPath

	assigned, maxPar, err := kahnWaves(order, preds, &result)
	if err != nil {
		return wavePlanResult{}, false
	}

	// DAG assignment — timeline-derived prRef is already on the node, so
	// pickDAG will prefer pr-feedback without an extra HTTP call.
	s.assignDAGParams(ctx, src, nodes, parentRepo, &result)

	result.Waves = buildWaveTickets(assigned, nodes, order)
	result.Parallelism = maxPar

	dependents := make(map[string]int)
	for _, ps := range preds {
		for p := range ps {
			dependents[p]++
		}
	}
	for id := range nodes {
		if len(preds[id]) == 0 && dependents[id] == 0 {
			result.Diagnostics.Orphans = append(result.Diagnostics.Orphans, id)
		}
	}
	sort.Strings(result.Diagnostics.Orphans)
	return result, true
}

// --- Wave Launch ---

type waveLaunchArgs struct {
	Epic    string            `json:"epic"`
	Source  *wavePlanSourceIn `json:"source"`
	Wave    *int              `json:"wave"`
	DAG     string            `json:"dag"`
	Tickets []string          `json:"tickets"`
	DryRun  bool              `json:"dry_run"`
}

type launchedTicket struct {
	Ticket        string `json:"ticket"`
	CorrelationID string `json:"correlation_id"`
	Workspace     string `json:"workspace,omitempty"`
}

type waveLaunchResult struct {
	Wave     int              `json:"wave"`
	Launched []launchedTicket `json:"launched"`
	Skipped  []string         `json:"skipped,omitempty"`
	Errors   []string         `json:"errors,omitempty"`
	DryRun   bool             `json:"dry_run"`
}

func (s *Server) toolWaveLaunch(ctx context.Context, raw json.RawMessage) (any, error) {
	var args waveLaunchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	src, err := parseWaveSource(wavePlanArgs{Epic: args.Epic, Source: args.Source})
	if err != nil {
		return nil, err
	}
	if args.DAG == "" {
		if src.Kind == "github" {
			args.DAG = "workspace-dev"
		} else {
			args.DAG = "jira-dev"
		}
	}

	plan, err := s.computeWavePlanForSource(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("compute wave plan: %w", err)
	}

	// Find the target wave.
	targetWave := 0
	if args.Wave != nil {
		targetWave = *args.Wave
	} else {
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

	// Filter to requested IDs if specified.
	if len(args.Tickets) > 0 {
		filter := make(map[string]bool)
		for _, t := range args.Tickets {
			filter[t] = true
		}
		var filtered []wavePlanTicket
		for _, t := range waveTickets {
			if filter[t.ID] || filter[t.Key] {
				filtered = append(filtered, t)
			}
		}
		waveTickets = filtered
	}

	result := waveLaunchResult{Wave: targetWave, DryRun: args.DryRun}

	for _, ticket := range waveTickets {
		if args.DryRun {
			result.Launched = append(result.Launched, launchedTicket{
				Ticket:    ticket.ID,
				Workspace: fmt.Sprintf("$RICK_REPOS_PATH/%s-rick-ws-%s", sanitizeRepoForWorkspace(ticket.Repo), workspaceSuffix(ticket)),
			})
			continue
		}

		launchParams := buildLaunchParams(ticket, args.DAG)
		wfArgs, _ := json.Marshal(launchParams)

		wfResult, wfErr := s.toolRunWorkflow(ctx, wfArgs)
		if wfErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", ticket.ID, wfErr))
			continue
		}

		if wfr, ok := wfResult.(runWorkflowResult); ok {
			result.Launched = append(result.Launched, launchedTicket{
				Ticket:        ticket.ID,
				CorrelationID: wfr.CorrelationID,
			})
		}
	}

	return result, nil
}

// buildLaunchParams shapes the rick_run_workflow input for a single wave
// child. GitHub children pass a source tag so rick_wave_status can look them
// up by tag; Jira children keep the legacy ticket-based path. When the plan
// already resolved a per-child DAG (via dag_map or PR linkage), it takes
// precedence over the caller-wide default.
func buildLaunchParams(ticket wavePlanTicket, dag string) map[string]any {
	effectiveDAG := dag
	if v, ok := ticket.DAGParams["dag"].(string); ok && v != "" {
		effectiveDAG = v
	}
	switch ticket.IDKind {
	case "github":
		owner, repo, number := splitGHID(ticket.ID)
		prompt := fmt.Sprintf("Implement GitHub issue %s: %s", ticket.ID, ticket.Summary)
		if ticket.Body != "" {
			prompt += "\n\nFull issue body below:\n\n" + ticket.Body
		}
		params := map[string]any{
			"prompt": prompt,
			"dag":    effectiveDAG,
			"source": fmt.Sprintf("gh:%s/%s#%d", owner, repo, number),
		}
		if ticket.Repo != "" {
			params["repo"] = ticket.Repo
		}
		// pr-feedback needs the PR source string from the plan.
		if effectiveDAG == "pr-feedback" {
			if pr, ok := ticket.DAGParams["pr_source"].(string); ok && pr != "" {
				params["source"] = pr
			}
		}
		return params
	default: // "jira"
		params := map[string]any{
			"prompt": fmt.Sprintf("Implement Jira ticket %s: %s", ticket.ID, ticket.Summary),
			"dag":    effectiveDAG,
			"ticket": ticket.ID,
		}
		if ticket.Repo != "" {
			params["repo"] = ticket.Repo
		}
		return params
	}
}

// splitGHID decomposes "owner/repo#N" into its parts; returns zero values on
// malformed input (callers have already validated via planning).
func splitGHID(id string) (string, string, int) {
	slash := strings.Index(id, "/")
	hash := strings.Index(id, "#")
	if slash <= 0 || hash <= slash+1 {
		return "", "", 0
	}
	owner := id[:slash]
	repo := id[slash+1 : hash]
	n, _ := strconv.Atoi(id[hash+1:])
	return owner, repo, n
}

// workspaceSuffix returns a short per-child suffix used in dry-run display and
// cleanup matching. Jira children use the key; GitHub children use "gh<N>".
func workspaceSuffix(t wavePlanTicket) string {
	if t.IDKind == "github" {
		_, _, n := splitGHID(t.ID)
		return fmt.Sprintf("gh%d", n)
	}
	return t.ID
}

// sanitizeRepoForWorkspace replaces "/" with "-" so owner/repo can appear in
// a directory name.
func sanitizeRepoForWorkspace(repo string) string {
	return strings.ReplaceAll(repo, "/", "-")
}

// --- Wave Status ---

type waveStatusArgs struct {
	Epic   string            `json:"epic"`
	Source *wavePlanSourceIn `json:"source"`
	Wave   *int              `json:"wave"`
}

type waveTicketStatus struct {
	Key            string `json:"key"`
	ID             string `json:"id"`
	IDKind         string `json:"id_kind"`
	WorkflowStatus string `json:"workflow_status,omitempty"`
	Phase          string `json:"phase,omitempty"`
	JiraStatus     string `json:"jira_status,omitempty"`
	IssueState     string `json:"issue_state,omitempty"`
}

type waveStatusResult struct {
	Epic      string             `json:"epic,omitempty"`
	Source    wavePlanSource     `json:"source"`
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
	src, err := parseWaveSource(wavePlanArgs{Epic: args.Epic, Source: args.Source})
	if err != nil {
		return nil, err
	}

	plan, err := s.computeWavePlanForSource(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("compute wave plan: %w", err)
	}

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

	result := waveStatusResult{Epic: plan.Epic, Source: plan.Source}
	if args.Wave != nil {
		result.Wave = *args.Wave
	}

	for _, wave := range targetWaves {
		for _, ticket := range wave.Tickets {
			ts := waveTicketStatus{
				Key:    ticket.Key,
				ID:     ticket.ID,
				IDKind: ticket.IDKind,
			}
			if ticket.IDKind == "jira" {
				ts.JiraStatus = ticket.Status
			} else {
				ts.IssueState = ticket.Status
			}

			tagKey, tagValue := waveStatusTag(ticket)
			correlations, tagErr := s.deps.Store.LoadByTag(ctx, tagKey, tagValue)
			if tagErr == nil && len(correlations) > 0 {
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

// waveStatusTag returns the (key, value) pair used to look up the workflow
// correlation for a given child. Jira children are indexed by their ticket
// key; GitHub children are indexed by their source tag.
func waveStatusTag(t wavePlanTicket) (string, string) {
	if t.IDKind == "github" {
		owner, repo, number := splitGHID(t.ID)
		return "source", fmt.Sprintf("gh:%s/%s#%d", owner, repo, number)
	}
	return "ticket", t.ID
}

// --- Wave Cleanup ---

type waveCleanupArgs struct {
	Epic   string            `json:"epic"`
	Source *wavePlanSourceIn `json:"source"`
	Wave   *int              `json:"wave"`
	Force  bool              `json:"force"`
}

func (s *Server) toolWaveCleanup(ctx context.Context, raw json.RawMessage) (any, error) {
	var args waveCleanupArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	src, err := parseWaveSource(wavePlanArgs{Epic: args.Epic, Source: args.Source})
	if err != nil {
		return nil, err
	}

	plan, err := s.computeWavePlanForSource(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("compute wave plan: %w", err)
	}

	reposPath := os.Getenv("RICK_REPOS_PATH")
	if reposPath == "" {
		return nil, fmt.Errorf("RICK_REPOS_PATH environment variable is not set")
	}

	// Collect match tokens — Jira children use their key, GitHub children use
	// their "-gh<N>-" workspace suffix so we never accidentally match a bare
	// issue number substring in an unrelated directory name.
	matchers := make([]string, 0)
	for _, wave := range plan.Waves {
		if args.Wave != nil && wave.Wave != *args.Wave {
			continue
		}
		for _, t := range wave.Tickets {
			if t.IDKind == "github" {
				_, _, n := splitGHID(t.ID)
				matchers = append(matchers, fmt.Sprintf("-gh%d-", n))
			} else {
				matchers = append(matchers, t.ID)
			}
		}
	}

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
		for _, token := range matchers {
			if !strings.Contains(name, token) {
				continue
			}
			path := filepath.Join(reposPath, name)
			if _, safeErr := safeWorkspacePath(path); safeErr != nil {
				skipped = append(skipped, fmt.Sprintf("%s: safety check failed: %v", name, safeErr))
				break
			}
			if rmErr := os.RemoveAll(path); rmErr != nil {
				skipped = append(skipped, fmt.Sprintf("%s: %v", name, rmErr))
			} else {
				cleaned = append(cleaned, name)
			}
			break
		}
	}

	return map[string]any{
		"source":  plan.Source,
		"epic":    plan.Epic,
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
