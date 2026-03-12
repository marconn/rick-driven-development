package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/marconn/rick-event-driven-development/internal/backend"
	"github.com/marconn/rick-event-driven-development/internal/event"
)

// ResearcherHandler is the codebase-researcher handler.
// It investigates target repositories to find files, patterns, and services
// relevant to the BTU requirements. Uses AI backends with tool permissions
// for deep codebase exploration.
type ResearcherHandler struct {
	backends []backend.Backend
	msMap    *MicroserviceMap
	state    *PlanningState
	logger   *slog.Logger
}

// NewResearcher creates a codebase-researcher handler.
func NewResearcher(backends []backend.Backend, msMap *MicroserviceMap, state *PlanningState, logger *slog.Logger) *ResearcherHandler {
	return &ResearcherHandler{backends: backends, msMap: msMap, state: state, logger: logger}
}

func (r *ResearcherHandler) Name() string            { return "codebase-researcher" }
func (r *ResearcherHandler) Subscribes() []event.Type { return nil }

// Handle processes the PersonaCompleted{confluence-reader} event.
func (r *ResearcherHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	wp := r.state.Get(env.CorrelationID)
	wp.mu.RLock()
	title := wp.BTUTitle
	content := wp.BTUContent
	microservices := wp.Microservices
	wp.mu.RUnlock()

	if content == "" {
		return nil, fmt.Errorf("codebase-researcher: no BTU content in state for %s", env.CorrelationID)
	}

	r.logger.Info("starting codebase research",
		slog.String("btu", title),
		slog.Int("microservices", len(microservices)),
	)

	// Resolve microservices to local repo paths
	found, missing := r.msMap.ResolveAll(microservices)
	if len(missing) > 0 {
		r.logger.Warn("microservices not found locally", slog.Any("missing", missing))
	}

	// Fallback: when BTU doesn't declare microservices in [brackets],
	// use AI to infer which repos from RICK_REPOS_PATH are relevant.
	if len(found) == 0 {
		r.logger.Warn("no repos from BTU detection, falling back to AI-assisted discovery")
		inferred := r.inferRepos(ctx, title, content)
		if len(inferred) > 0 {
			found, _ = r.msMap.ResolveAll(inferred)
			r.logger.Info("AI inferred repos", slog.Int("count", len(found)), slog.Any("repos", inferred))
		}
	}
	if len(found) == 0 {
		r.logger.Warn("no repos found after inference, researching with BTU content only")
	}

	// Phase 1: Parallel research across repos and AI backends
	findings := r.parallelResearch(ctx, title, content, found)

	// Phase 2: Consolidation — merge findings and identify gaps
	consolidated := r.consolidate(ctx, title, findings)

	// Store in shared state
	wp.mu.Lock()
	wp.ResearchFindings = consolidated
	wp.mu.Unlock()

	r.logger.Info("research complete",
		slog.Int("findings_len", len(consolidated)),
	)

	enrichEvt := event.New(event.ContextEnrichment, 1, event.MustMarshal(event.ContextEnrichmentPayload{
		Source:  "codebase-researcher",
		Kind:    "research",
		Summary: fmt.Sprintf("Researched %d repos, consolidated findings (%d chars)", len(found), len(consolidated)),
	})).WithSource("handler:codebase-researcher")

	return []event.Envelope{enrichEvt}, nil
}

// inferRepos uses a lightweight AI call to identify which repos from RICK_REPOS_PATH
// are likely relevant to the BTU when the confluence-reader found no [bracket] microservices.
func (r *ResearcherHandler) inferRepos(ctx context.Context, title, content string) []string {
	if len(r.backends) == 0 {
		return nil
	}

	available := r.msMap.ListAvailable()
	if len(available) == 0 {
		return nil
	}

	// Cap the list to avoid an absurdly long prompt (keep first 200).
	if len(available) > 200 {
		available = available[:200]
	}

	// Include platform context (AGENTS.md) if available — has domain guide,
	// BFF dependency map, and repo descriptions that help AI select repos.
	platformCtx := ""
	if pc := r.msMap.PlatformContext(); pc != "" {
		platformCtx = "\n## Platform Context (Architecture & Domain Guide)\n" + pc + "\n"
	}

	prompt := fmt.Sprintf(`Given this BTU (feature request), identify which repositories from the list below are likely involved in the implementation. Return ONLY a JSON array of repo names, nothing else.

## BTU: %s

%s
%s
## Available Repositories
%s

Return a JSON array of the 2-6 most relevant repository names. Example: ["frontend-emr", "bff-emr", "backend-api"]
Only include repos that would need code changes for this BTU.`, title, content, platformCtx, strings.Join(available, "\n"))

	resp, err := r.backends[0].Run(ctx, backend.Request{
		SystemPrompt: "You select relevant repositories for a feature. Return ONLY a JSON array of strings.",
		UserPrompt:   prompt,
	})
	if err != nil {
		r.logger.Warn("repo inference failed", slog.Any("error", err))
		return nil
	}

	// Parse JSON array from output (may be wrapped in markdown code block).
	output := strings.TrimSpace(resp.Output)
	output = strings.TrimPrefix(output, "```json")
	output = strings.TrimPrefix(output, "```")
	output = strings.TrimSuffix(output, "```")
	output = strings.TrimSpace(output)

	var repos []string
	if err := json.Unmarshal([]byte(output), &repos); err != nil {
		r.logger.Warn("repo inference: failed to parse JSON", slog.String("output", output), slog.Any("error", err))
		return nil
	}

	return repos
}

type researchResult struct {
	microservice string
	findings     string
	err          error
}

// parallelResearch dispatches research tasks across backends and repos concurrently.
func (r *ResearcherHandler) parallelResearch(ctx context.Context, title, content string, repos map[string]string) []researchResult {
	var (
		mu      sync.Mutex
		results []researchResult
		wg      sync.WaitGroup
	)

	msListStr := formatMicroserviceList(repos)

	for ms, repoPath := range repos {
		for _, be := range r.backends {
			wg.Add(1)
			go func(ms, repoPath string, be backend.Backend) {
				defer wg.Done()

				prompt := renderTemplate(ResearchUserPromptTemplate, map[string]string{
					"BTUTitle":      title,
					"BTUContent":    content,
					"Microservices": msListStr,
					"RepoPath":      repoPath,
				})

				r.logger.Debug("researching repo",
					slog.String("microservice", ms),
					slog.String("repo", repoPath),
				)

				resp, err := be.Run(ctx, backend.Request{
					SystemPrompt: ResearchSystemPrompt,
					UserPrompt:   prompt,
					WorkDir:      repoPath,
					Yolo:         true, // needs file access for research
				})

				var output string
				if resp != nil {
					output = resp.Output
				}

				mu.Lock()
				results = append(results, researchResult{
					microservice: ms,
					findings:     output,
					err:          err,
				})
				mu.Unlock()
			}(ms, repoPath, be)
		}
	}

	wg.Wait()
	return results
}

// consolidate merges research findings from multiple backends/repos into a single report.
func (r *ResearcherHandler) consolidate(ctx context.Context, title string, results []researchResult) string {
	if len(results) == 0 {
		return "No codebase research was possible -- no repos found locally."
	}

	// If only one result and no error, use it directly
	if len(results) == 1 && results[0].err == nil {
		return results[0].findings
	}

	// Build consolidated input
	var sb strings.Builder
	for _, res := range results {
		if res.err != nil {
			fmt.Fprintf(&sb, "\n## %s (ERROR)\n%s\n", res.microservice, res.err)
			continue
		}
		fmt.Fprintf(&sb, "\n## %s\n%s\n", res.microservice, res.findings)
	}

	// If we have multiple backends, ask AI to consolidate
	if len(r.backends) > 0 {
		consolidationPrompt := fmt.Sprintf(`Consolida los siguientes hallazgos de investigacion del codebase para el BTU "%s".

Elimina duplicados, resuelve contradicciones, y produce un reporte unificado con:
- Archivos a modificar (rutas exactas, sin duplicados)
- Patrones existentes relevantes
- Dependencias de API/servicio
- Modelos de datos
- Riesgos tecnicos
- Piezas faltantes

Hallazgos por microservicio:
%s`, title, sb.String())

		resp, err := r.backends[0].Run(ctx, backend.Request{
			SystemPrompt: "You are a senior software engineer consolidating codebase research findings. Output in Spanish.",
			UserPrompt:   consolidationPrompt,
		})
		if err == nil {
			return resp.Output
		}
		r.logger.Warn("consolidation failed, using raw findings", slog.Any("error", err))
	}

	return sb.String()
}

func formatMicroserviceList(repos map[string]string) string {
	var sb strings.Builder
	for ms, path := range repos {
		fmt.Fprintf(&sb, "- %s -> %s\n", ms, path)
	}
	return sb.String()
}
