package handler

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
	"github.com/marconn/rick-event-driven-development/internal/eventstore"
)

// Caps keep the sweep cheap and the consolidated review readable. When either
// is hit the handler logs what it dropped — silent truncation would read as
// "checked everything" when it didn't (CLAUDE.md §3, no-silent-caps).
const (
	staleRefMaxSymbols  = 50
	staleRefMaxFindings = 30
)

// PRStaleReferenceHandler is a deterministic, non-AI sweep that closes the
// cross-file gap of the pr-docs-concordance reviewer: it finds references to
// symbols this PR renamed or deleted that still live in files the PR does not
// touch (docs and code comments). A git-grep hit is ground truth — the symbol
// provably exists at that file:line — so unlike the AI reviewers this handler
// needs no diff-grounding pass. Findings ride into the consolidated PR review
// as advisory, non-blocking notes via a ContextEnrichment the consolidator
// folds in (see pr_consolidator.go). Opt-in via RICK_ENABLE_STALE_REF_SWEEP.
type PRStaleReferenceHandler struct {
	store  eventstore.Store
	logger *slog.Logger
}

// NewPRStaleReference creates a PRStaleReferenceHandler from the shared Deps.
func NewPRStaleReference(d Deps) *PRStaleReferenceHandler {
	return &PRStaleReferenceHandler{store: d.Store, logger: d.Logger}
}

// Name returns the unique handler identifier.
func (h *PRStaleReferenceHandler) Name() string { return "pr-stale-reference" }

// Subscribes returns empty — DAG-based dispatch handles subscriptions.
func (h *PRStaleReferenceHandler) Subscribes() []event.Type { return nil }

// Handle loads the PR workspace, extracts the symbols this PR removed, greps
// the unchanged files for lingering references, and emits a ContextEnrichment
// describing the stale ones. Returns nil (no enrichment) when there is nothing
// to report or the workspace is unavailable — this handler never blocks the
// review.
func (h *PRStaleReferenceHandler) Handle(ctx context.Context, env event.Envelope) ([]event.Envelope, error) {
	ws, err := h.loadWorkspaceReady(ctx, env.CorrelationID)
	if err != nil {
		return nil, fmt.Errorf("pr-stale-reference: load workspace ready: %w", err)
	}
	if ws == nil || ws.Path == "" || ws.Base == "" {
		// No workspace/base to diff against — nothing this handler can do.
		return nil, nil
	}

	diff, files, ok := generateWorkspaceDiff(ctx, ws.Path, ws.Base)
	if !ok {
		return nil, nil
	}

	changed := make(map[string]struct{}, len(files))
	for _, f := range files {
		changed[f.Path] = struct{}{}
	}

	symbols := extractRemovedSymbols(diff)
	if len(symbols) > staleRefMaxSymbols {
		h.logf("pr-stale-reference: symbol cap hit, dropping candidates",
			"total", len(symbols), "kept", staleRefMaxSymbols)
		symbols = symbols[:staleRefMaxSymbols]
	}
	if len(symbols) == 0 {
		return nil, nil
	}

	findings := h.sweep(ctx, ws.Path, symbols, changed)
	capped := false
	if len(findings) > staleRefMaxFindings {
		h.logf("pr-stale-reference: finding cap hit, truncating output",
			"total", len(findings), "kept", staleRefMaxFindings)
		findings = findings[:staleRefMaxFindings]
		capped = true
	}
	if len(findings) == 0 {
		return nil, nil
	}

	enrichment := event.ContextEnrichmentPayload{
		Source:  "pr-stale-reference",
		Kind:    "stale-references",
		Summary: formatStaleReferences(findings, capped),
	}
	evt := event.New(event.ContextEnrichment, 1, event.MustMarshal(enrichment)).
		WithSource("handler:pr-stale-reference")
	return []event.Envelope{evt}, nil
}

// staleRefFinding is one lingering reference to a removed symbol.
type staleRefFinding struct {
	Symbol  string
	File    string
	Line    int
	Snippet string
}

// sweep greps the workspace for each candidate symbol and keeps hits in
// unchanged files that are either documentation or code comments. A renamed
// symbol still referenced in unchanged *executable* code is a compile/semantic
// concern other layers own, not documentation rot — so non-comment code hits
// are deliberately dropped.
func (h *PRStaleReferenceHandler) sweep(ctx context.Context, wsPath string, symbols []string, changed map[string]struct{}) []staleRefFinding {
	var findings []staleRefFinding
	for _, sym := range symbols {
		for _, hit := range gitGrepSymbol(ctx, wsPath, sym) {
			if _, isChanged := changed[hit.File]; isChanged {
				continue
			}
			if !isStaleRefDocFile(hit.File) && !isCommentLine(hit.Snippet) {
				continue
			}
			hit.Symbol = sym
			findings = append(findings, hit)
		}
	}
	return findings
}

func (h *PRStaleReferenceHandler) logf(msg string, args ...any) {
	if h.logger != nil {
		h.logger.Warn(msg, args...)
	}
}

// loadWorkspaceReady scans the correlation chain for the WorkspaceReady event.
func (h *PRStaleReferenceHandler) loadWorkspaceReady(ctx context.Context, correlationID string) (*event.WorkspaceReadyPayload, error) {
	if h.store == nil || correlationID == "" {
		return nil, nil
	}
	events, err := h.store.LoadByCorrelation(ctx, correlationID)
	if err != nil {
		return nil, fmt.Errorf("load correlation chain: %w", err)
	}
	for _, e := range events {
		if e.Type != event.WorkspaceReady {
			continue
		}
		var p event.WorkspaceReadyPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fmt.Errorf("unmarshal workspace ready: %w", err)
		}
		return &p, nil
	}
	return nil, nil
}

// identRe matches Go-style identifiers; pathRe matches quoted path/key literals
// like "internal/handler" or "context.enrichment".
var (
	identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	pathRe  = regexp.MustCompile(`[A-Za-z0-9_]+[./][A-Za-z0-9._/-]+`)
)

// extractRemovedSymbols parses a unified diff and returns the high-signal
// identifiers a PR removed from a file without re-adding them (the rename/
// delete signal). The removed-minus-added-per-file rule is the primary
// precision mechanism: a token edited in place (present on both - and + lines)
// is not a rename and is excluded. isCandidateSymbol applies a conservative
// shape filter on top.
func extractRemovedSymbols(diff string) []string {
	type fileTokens struct {
		removed map[string]struct{}
		added   map[string]struct{}
	}
	perFile := map[string]*fileTokens{}
	current := ""
	ensure := func() *fileTokens {
		if current == "" {
			current = "(unknown)"
		}
		ft, ok := perFile[current]
		if !ok {
			ft = &fileTokens{removed: map[string]struct{}{}, added: map[string]struct{}{}}
			perFile[current] = ft
		}
		return ft
	}

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			current = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- "):
			// File headers — ignore (the "+++ b/" case above already captured the path).
		case strings.HasPrefix(line, "-"):
			ft := ensure()
			for _, tok := range tokenize(line[1:]) {
				ft.removed[tok] = struct{}{}
			}
		case strings.HasPrefix(line, "+"):
			ft := ensure()
			for _, tok := range tokenize(line[1:]) {
				ft.added[tok] = struct{}{}
			}
		}
	}

	candidates := map[string]struct{}{}
	for _, ft := range perFile {
		for tok := range ft.removed {
			if _, readded := ft.added[tok]; readded {
				continue
			}
			if isCandidateSymbol(tok) {
				candidates[tok] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(candidates))
	for tok := range candidates {
		out = append(out, tok)
	}
	sort.Strings(out) // deterministic order for stable tests + output
	return out
}

// tokenize extracts candidate identifiers and quoted path/key literals from a
// single source line.
func tokenize(s string) []string {
	toks := identRe.FindAllString(s, -1)
	toks = append(toks, pathRe.FindAllString(s, -1)...)
	return toks
}

// isCandidateSymbol applies the conservative shape filter. Only three shapes
// survive, each chosen because it is specific enough that a cross-repo grep
// won't drown in false positives:
//   - multi-word exported CamelCase (≥2 uppercase, has lowercase, len≥5):
//     FetchUser, WorkspaceReady, PRStaleReference — but NOT Error/String/Handler.
//   - SCREAMING_SNAKE consts / env vars (all caps+digits+underscore, len≥5).
//   - quoted path/key literals containing '/' or '.' (len≥5).
func isCandidateSymbol(tok string) bool {
	if len(tok) < 5 {
		return false
	}
	if strings.ContainsAny(tok, "/.") {
		return true // path/key literal (pathRe already constrained the charset)
	}
	if isScreamingSnake(tok) {
		return true
	}
	return isMultiWordExported(tok)
}

func isScreamingSnake(tok string) bool {
	hasUnderscore := false
	for _, r := range tok {
		switch {
		case r == '_':
			hasUnderscore = true
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return hasUnderscore
}

func isMultiWordExported(tok string) bool {
	r0 := tok[0]
	if r0 < 'A' || r0 > 'Z' {
		return false
	}
	uppers, hasLower := 0, false
	for _, r := range tok {
		switch {
		case r >= 'A' && r <= 'Z':
			uppers++
		case r >= 'a' && r <= 'z':
			hasLower = true
		}
	}
	return uppers >= 2 && hasLower
}

// docExts are the documentation file extensions whose grep hits are always
// accepted (no comment-line check).
var docExts = map[string]struct{}{
	".md": {}, ".txt": {}, ".rst": {}, ".adoc": {}, ".markdown": {},
}

func isStaleRefDocFile(path string) bool {
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return false
	}
	_, ok := docExts[strings.ToLower(path[dot:])]
	return ok
}

// commentPrefixes covers the common single-line/block comment markers across
// the languages in this org (Go, JS/TS, PHP, Python, shell, SQL, Lua, INI,
// HTML, C-family). A hit in a non-doc file is only accepted when its line is a
// comment.
var commentPrefixes = []string{"//", "#", "*", "/*", "--", ";", "<!--", "\"\"\"", "'''"}

func isCommentLine(snippet string) bool {
	t := strings.TrimSpace(snippet)
	for _, p := range commentPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// gitGrepSymbol runs a whole-word, fixed-string grep for sym across the tracked
// files in the workspace. Exit code 1 (no match) is normal and yields no hits.
func gitGrepSymbol(ctx context.Context, wsPath, sym string) []staleRefFinding {
	// -n line numbers, -w whole word, -F fixed string, -I skip binaries.
	cmd := exec.CommandContext(ctx, "git", "-C", wsPath, "grep", "-nwFI", "--", sym)
	out, err := cmd.Output()
	if err != nil {
		// Exit status 1 = no matches; anything else (no repo, bad arg) we
		// simply treat as no findings — this handler must never fail the review.
		return nil
	}
	var findings []staleRefFinding
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		f, ok := parseGitGrepLine(sc.Text())
		if ok {
			findings = append(findings, f)
		}
	}
	return findings
}

// parseGitGrepLine parses one "path:line:content" record from `git grep -n`.
// Paths never contain ':' in this repo; the line number is the first numeric
// field after the path.
func parseGitGrepLine(s string) (staleRefFinding, bool) {
	first := strings.IndexByte(s, ':')
	if first < 0 {
		return staleRefFinding{}, false
	}
	rest := s[first+1:]
	second := strings.IndexByte(rest, ':')
	if second < 0 {
		return staleRefFinding{}, false
	}
	path := s[:first]
	lineNum := rest[:second]
	content := rest[second+1:]
	n := 0
	for _, r := range lineNum {
		if r < '0' || r > '9' {
			return staleRefFinding{}, false
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return staleRefFinding{}, false
	}
	return staleRefFinding{File: path, Line: n, Snippet: strings.TrimSpace(content)}, true
}

// formatStaleReferences renders the findings into the markdown the consolidator
// folds into its review body. It states plainly that these are advisory.
func formatStaleReferences(findings []staleRefFinding, capped bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This PR renamed or removed symbols that are still referenced in unchanged files (advisory — verify and update or confirm intentional):\n\n")
	for _, f := range findings {
		snippet := f.Snippet
		if len(snippet) > 160 {
			snippet = snippet[:160] + "…"
		}
		fmt.Fprintf(&b, "- `%s` — `%s:%d`: %s\n", f.Symbol, f.File, f.Line, snippet)
	}
	if capped {
		fmt.Fprintf(&b, "\n(Output truncated at %d findings; more references may exist.)\n", staleRefMaxFindings)
	}
	return b.String()
}
