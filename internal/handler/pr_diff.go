package handler

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/marconn/rick-event-driven-development/internal/event"
)

// prDiffMaxBytes caps the unified diff injected into reviewer prompts. Sized
// to fit alongside the system prompt + phase template + source prompt inside
// the narrower backend context window (Claude Sonnet ~200k tokens). Giant
// PRs truncate past this cap; the per-file manifest still covers all files.
const prDiffMaxBytes = 512 * 1024

// diffFile is a single per-file entry from `git diff --numstat`.
type diffFile struct {
	Path    string
	Added   int
	Removed int
	// Binary is true when `--numstat` reports "-\t-\t<path>" (unified diff
	// carries no line counts for binaries).
	Binary bool
}

// generateWorkspaceDiff runs `git diff origin/<base>...HEAD` inside the
// cloned PR workspace and returns the full unified diff plus the per-file
// manifest. Uses three-dot (merge-base) to match GitHub's "Files changed"
// semantic. This path has no vendor size cap — the prior `gh pr diff`
// approach returned HTTP 406 for PRs with >300 files, silently producing
// an empty review.
func generateWorkspaceDiff(ctx context.Context, workspacePath, base string) (diff string, files []diffFile, ok bool) {
	if workspacePath == "" || base == "" {
		return "", nil, false
	}

	// Sanity check: confirm we're inside a git work tree before running diff.
	if err := exec.CommandContext(ctx, "git", "-C", workspacePath, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return "", nil, false
	}

	spec := "origin/" + base + "...HEAD"

	nsOut, err := exec.CommandContext(ctx, "git", "-C", workspacePath, "diff", "--numstat", spec).Output()
	if err != nil {
		return "", nil, false
	}
	for _, line := range strings.Split(string(nsOut), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		removed, _ := strconv.Atoi(parts[1])
		files = append(files, diffFile{
			Path:    parts[2],
			Added:   added,
			Removed: removed,
			Binary:  parts[0] == "-",
		})
	}

	dOut, err := exec.CommandContext(ctx, "git", "-C", workspacePath, "diff", spec).Output()
	if err != nil {
		return "", files, false
	}

	return string(dOut), files, true
}

// buildWorkspaceDiffEnrichment renders the workspace diff into a
// ContextEnrichment ready for the reviewer prompt. The per-file manifest is
// always complete; only the unified-diff body is capped. Kind flips to
// "pr-diff-truncated" when the body was cut so the consolidator can
// downgrade a unanimous-pass review to COMMENT with an explicit caveat
// (an all-pass result on a partial diff is not a confident APPROVE).
func buildWorkspaceDiffEnrichment(ctx context.Context, workspacePath, base string) event.ContextEnrichmentPayload {
	diff, files, ok := generateWorkspaceDiff(ctx, workspacePath, base)
	if !ok {
		return event.ContextEnrichmentPayload{}
	}

	var totalAdded, totalRemoved int
	for _, f := range files {
		totalAdded += f.Added
		totalRemoved += f.Removed
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## PR Changed Files (%d files, +%d / -%d LOC)\n\n", len(files), totalAdded, totalRemoved)
	for _, f := range files {
		if f.Binary {
			fmt.Fprintf(&b, "- `%s` (binary)\n", f.Path)
			continue
		}
		fmt.Fprintf(&b, "- `%s` (+%d / -%d)\n", f.Path, f.Added, f.Removed)
	}

	truncated := false
	body := diff
	if len(body) > prDiffMaxBytes {
		truncated = true
		body = body[:prDiffMaxBytes]
		// Trim back to the last newline so we don't cut mid-hunk.
		if idx := strings.LastIndexByte(body, '\n'); idx > 0 {
			body = body[:idx]
		}
	}

	if truncated {
		fmt.Fprintf(&b,
			"\n## PR Diff (TRUNCATED after %d bytes — review is partial. File manifest above is complete; diff body below covers only the first portion of the changes.)\n\n```diff\n%s\n```\n",
			prDiffMaxBytes, body)
	} else {
		fmt.Fprintf(&b, "\n## PR Diff\n\n```diff\n%s\n```\n", body)
	}

	kind := "pr-diff"
	if truncated {
		kind = "pr-diff-truncated"
	}

	return event.ContextEnrichmentPayload{
		Source:  "pr-workspace",
		Kind:    kind,
		Summary: b.String(),
	}
}

// fetchWorkspaceRawDiff returns the full unified diff from the workspace
// clone without truncation. Used by pr-consolidator to validate inline
// anchor positions — the consolidator needs the full byte sequence to find
// line numbers inside hunks, not the reviewer-truncated version. Empty
// return on any failure lets callers fall back to a secondary source.
func fetchWorkspaceRawDiff(ctx context.Context, workspacePath, base string) string {
	if workspacePath == "" || base == "" {
		return ""
	}
	if err := exec.CommandContext(ctx, "git", "-C", workspacePath, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, "git", "-C", workspacePath, "diff", "origin/"+base+"...HEAD").Output()
	if err != nil {
		return ""
	}
	return string(out)
}
