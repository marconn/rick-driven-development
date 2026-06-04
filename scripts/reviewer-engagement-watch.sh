#!/usr/bin/env bash
# reviewer-engagement-watch.sh — validate that manifest-driven pr-* reviewers
# engage at the same rate as the embedded prompts (epic #3, task #12 step 4).
#
# WHY THIS EXISTS: the first prod pr-review on manifest prompts dropped reviewer
# candidate-finding rate ~8x (a self-suppression clause in the diff-grounding
# skill, fixed in PR #20). The golden equivalence test could not catch a harmful
# *added* instruction — only this engagement telemetry did. Run this after
# re-enabling manifests to confirm the fixed skill restored engagement before
# declaring #12 done.
#
# METRIC: per pr-category reviewer invocation, verdict.grounding.summary records
# `original_count` = how many candidate findings the LLM produced *before* the
# code-side grounding filter ran. That is the purest engagement signal (the
# filter, not the prompt, decides what survives). We compare the mean and the
# zero-finding rate across three eras.
#
# Usage:  scripts/reviewer-engagement-watch.sh [path-to-rick.db]
# Default DB: the deployed store ($HOME/.local/share/rick/rick.db).

set -euo pipefail

DB="${1:-$HOME/.local/share/rick/rick.db}"

# Era boundaries (epic #3 timeline, America/Denver):
#   < EMBEDDED_END         : embedded prompts (the trusted 0.67 avg / 69% zero baseline)
#   BAD_START .. BAD_END   : manifest prompts with the over-suppressing diff-grounding skill
#   >= FIXED_START         : manifest prompts with the fixed (citation-only) skill  <-- the era we are validating
EMBEDDED_END="2026-06-03T21:43:00"
BAD_START="2026-06-03T21:43:00"
BAD_END="2026-06-03T23:15:00"
FIXED_START="2026-06-03T23:15:00"

if [[ ! -f "$DB" ]]; then
  echo "error: db not found: $DB" >&2
  exit 1
fi

echo "DB: $DB"
echo "Reviewer engagement by era (original_count = LLM candidate findings before the grounding filter)"
echo

sqlite3 -box "$DB" "
WITH gs AS (
  SELECT json_extract(payload,'\$.original_count') AS oc, timestamp AS ts
  FROM events WHERE type='verdict.grounding.summary'
)
SELECT
  CASE
    WHEN ts <  '$EMBEDDED_END'                          THEN '1. embedded (baseline)'
    WHEN ts >= '$BAD_START' AND ts < '$BAD_END'         THEN '2. manifest (over-suppressing skill)'
    WHEN ts >= '$FIXED_START'                            THEN '3. manifest (FIXED skill) <- validating'
    ELSE '?' END                                        AS era,
  COUNT(*)                                              AS reviewer_runs,
  ROUND(AVG(oc), 3)                                     AS avg_findings,
  ROUND(100.0*SUM(CASE WHEN oc=0 THEN 1 ELSE 0 END)/COUNT(*), 1) AS pct_zero_finding
FROM gs
GROUP BY era ORDER BY era;
"

echo
echo "Decision guide:"
echo "  - Era 3 needs ~5-10 pr-reviews (65-130 reviewer runs) before it is conclusive —"
echo "    the embedded baseline itself is ~69% zero-finding, so a single PR is within noise."
echo "  - PASS if era 3 avg_findings and pct_zero_finding land near era 1 (embedded baseline)."
echo "  - REGRESSED if era 3 looks like era 2 (avg << baseline, pct_zero >> baseline) — revisit the skills."
