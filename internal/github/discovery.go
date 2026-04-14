package github

import (
	"regexp"
	"strings"
)

// taskListPattern matches a Markdown task-list item referencing an issue.
// Captures: (1) checkbox state (space/x/X), (2) owner (optional), (3) repo
// (optional), (4) issue number. Anchored to line start because Markdown
// task-list items must begin the line (modulo leading spaces for indented
// sub-lists — we tolerate up to 6 spaces).
var taskListPattern = regexp.MustCompile(`(?m)^\s{0,6}[-*]\s+\[([ xX])\]\s+.*?(?:([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+))?#(\d+)`)

// ParseTaskList finds GitHub task-list items that reference issues.
// Checked rows ("[x]") are considered done and excluded — they don't belong
// in a wave plan. Returns refs in first-appearance order.
func ParseTaskList(body string) []IssueRef {
	if body == "" {
		return nil
	}
	matches := taskListPattern.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil
	}
	refs := make([]IssueRef, 0, len(matches))
	seen := make(map[string]bool)
	for _, m := range matches {
		checked := strings.EqualFold(m[1], "x")
		if checked {
			continue
		}
		ref := IssueRef{Owner: m[2], Repo: m[3]}
		if n := atoiOrZero(m[4]); n > 0 {
			ref.Number = n
		} else {
			continue
		}
		key := ref.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, ref)
	}
	return refs
}

// ParseBodyRefs scrapes a parent issue body for every `#N` / `owner/repo#N`
// reference. Uses the same tolerant pattern as dependency parsing; the caller
// is responsible for filtering self-references. Deduplicated, order preserved.
func ParseBodyRefs(body string) []IssueRef {
	raw := ParseIssueRefs(body)
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]IssueRef, 0, len(raw))
	for _, r := range raw {
		key := r.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

func atoiOrZero(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// depKeywordPattern matches one of GitHub's issue-linking keywords (Depends
// on, Blocks, Blocked by) and captures the trailing clause up to the next
// sentence-ending punctuation or newline — from which ParseIssueRefs extracts
// every referenced issue. Intentionally excludes "Closes/Fixes/Resolves"
// because those invert direction (the PR closes the issue — not a blocking
// relation for planning).
var depKeywordPattern = regexp.MustCompile(`(?i)\b(depends\s+on|blocked\s+by|blocks)\b([^.!?\n]*)`)

// ParseBodyDependencies extracts dependency edges from an issue body using
// GitHub keyword phrases. `self` is the body's own issue — edges where the
// referenced issue is self are dropped. `Blocks #N` means self blocks N, so
// the edge is (N depends on self); the other two point the other way.
func ParseBodyDependencies(self IssueRef, body string) []DependencyEdge {
	if body == "" {
		return nil
	}
	var edges []DependencyEdge
	for _, m := range depKeywordPattern.FindAllStringSubmatch(body, -1) {
		keyword := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
		for _, ref := range ParseIssueRefs(m[2]) {
			if ref.Number == self.Number && sameRepo(ref, self) {
				continue
			}
			switch keyword {
			case "depends on", "blocked by":
				edges = append(edges, DependencyEdge{From: self, On: ref})
			case "blocks":
				edges = append(edges, DependencyEdge{From: ref, On: self})
			}
		}
	}
	return edges
}

// sameRepo treats an empty (owner, repo) on one side as a wildcard — two refs
// that differ only in an omitted owner/repo are still the same repo.
func sameRepo(a, b IssueRef) bool {
	if a.Owner == "" || b.Owner == "" {
		return true
	}
	return a.Owner == b.Owner && a.Repo == b.Repo
}
