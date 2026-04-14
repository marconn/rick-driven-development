package github

import (
	"regexp"
	"strconv"
	"strings"
)

// IssueRef identifies a child issue that appears in a dependency table row.
// Repo is optional: empty means "same repo as the parent issue" and the caller
// resolves the fallback.
type IssueRef struct {
	Owner  string
	Repo   string
	Number int
}

// String returns the canonical "#N" or "owner/repo#N" form.
func (r IssueRef) String() string {
	if r.Owner == "" || r.Repo == "" {
		return "#" + strconv.Itoa(r.Number)
	}
	return r.Owner + "/" + r.Repo + "#" + strconv.Itoa(r.Number)
}

// DependencyEdge is a directed edge "From depends on On" — i.e. On must
// complete before From. Callers translate to a node → predecessor-set graph.
type DependencyEdge struct {
	From IssueRef
	On   IssueRef
}

// issueRefPattern matches either "#42" or "owner/repo#42" anywhere in a string.
// The capture groups are: (1) optional owner, (2) optional repo, (3) number.
var issueRefPattern = regexp.MustCompile(`(?:([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+))?#(\d+)`)

// ParseIssueRefs finds every issue reference (#N or owner/repo#N) in s.
// Duplicates are preserved in the order they appear — caller deduplicates if
// needed. An empty string yields a nil slice.
func ParseIssueRefs(s string) []IssueRef {
	if s == "" {
		return nil
	}
	matches := issueRefPattern.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	refs := make([]IssueRef, 0, len(matches))
	for _, m := range matches {
		num, err := strconv.Atoi(m[3])
		if err != nil || num <= 0 {
			continue
		}
		refs = append(refs, IssueRef{Owner: m[1], Repo: m[2], Number: num})
	}
	return refs
}

// ParseDependencyTable scans a Markdown body for a GitHub-flavored pipe table
// that contains an "Issue" column and a "Depends on" column, then returns one
// DependencyEdge per (row-issue, dependency) pair.
//
// Table shape tolerated (column order doesn't matter, headers are
// case-insensitive, extra columns are ignored):
//
//	| Issue | Depends on        | Notes   |
//	| ----- | ----------------- | ------- |
//	| #642  |                   | root    |
//	| #645  | #642, #646        | fan-in  |
//
// Returns nil when the body has no matching table or when the table has no
// rows with dependencies. Parsing is deliberately lenient: malformed rows are
// skipped rather than failing the whole parse — a single mis-typed cell
// shouldn't kill a wave plan.
func ParseDependencyTable(body string) []DependencyEdge {
	if body == "" {
		return nil
	}

	lines := strings.Split(body, "\n")
	// Walk every line looking for a header row: "| ... | ... |".
	for i := 0; i < len(lines); i++ {
		cols := splitTableRow(lines[i])
		if len(cols) < 2 {
			continue
		}
		issueIdx, depIdx := findIssueColumns(cols)
		if issueIdx < 0 || depIdx < 0 {
			continue
		}
		// Next line must be a separator ("|---|---|") for this to be a
		// Markdown table header.
		if i+1 >= len(lines) || !isSeparatorRow(lines[i+1]) {
			continue
		}
		// Consume data rows until a non-row line.
		var edges []DependencyEdge
		for j := i + 2; j < len(lines); j++ {
			row := splitTableRow(lines[j])
			if len(row) == 0 {
				break
			}
			if issueIdx >= len(row) || depIdx >= len(row) {
				continue
			}
			from := firstIssueRef(row[issueIdx])
			if from == nil {
				continue
			}
			for _, dep := range ParseIssueRefs(row[depIdx]) {
				edges = append(edges, DependencyEdge{From: *from, On: dep})
			}
		}
		return edges
	}
	return nil
}

// splitTableRow splits a Markdown table row by "|" and trims cells.
// Returns nil when the line has fewer than two pipe separators (i.e. not a
// plausible row).
func splitTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, "|") {
		return nil
	}
	// Strip leading/trailing pipes for consistent splitting.
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	if len(parts) < 2 {
		return nil
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimSpace(p)
	}
	return out
}

func isSeparatorRow(line string) bool {
	cols := splitTableRow(line)
	if len(cols) == 0 {
		return false
	}
	for _, c := range cols {
		// A separator cell is made of dashes and optional colons (alignment).
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' && r != ':' && r != ' ' {
				return false
			}
		}
	}
	return true
}

// findIssueColumns returns (issueIdx, dependsIdx) from a header row.
// Either index is -1 if the corresponding column is not present.
func findIssueColumns(header []string) (int, int) {
	issueIdx, depIdx := -1, -1
	for i, cell := range header {
		lc := strings.ToLower(cell)
		switch {
		case issueIdx < 0 && (lc == "issue" || lc == "id" || lc == "ticket" || lc == "task" || lc == "child"):
			issueIdx = i
		case depIdx < 0 && strings.Contains(lc, "depends"):
			depIdx = i
		case depIdx < 0 && lc == "blocked by":
			depIdx = i
		}
	}
	return issueIdx, depIdx
}

// firstIssueRef returns the first "#N" or "owner/repo#N" in s, or nil when
// none is present.
func firstIssueRef(s string) *IssueRef {
	refs := ParseIssueRefs(s)
	if len(refs) == 0 {
		return nil
	}
	r := refs[0]
	return &r
}
