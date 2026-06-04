# skills (`skills/`)

Reusable skill fragments composed into persona system prompts. One subdir per skill (`SKILL.md`: YAML frontmatter + markdown body). A persona's manifest lists `skills:` in order; the resolver appends each skill's body after the identity.

## Format
- Frontmatter: `name`, `description`, optional `tools[]` (MCP tool allowlist, Claude only). Parsed by `../manifest.go` (`ParseSkillManifest`).
- Body: the skill instruction text (required).
- Same forbidden-key safety boundary as persona manifests.

## Current skills
- `diff-grounding` — every finding cites the exact changed file/line. (The shared reviewer citation rule, deduped from the 13 `pr-*` prompts.) **Citation only — it must NOT tell the model to withhold findings**; the diff-grounding *filter* (`handler/review.go`, code) decides anchoring. An earlier version added a "do not raise it" self-suppression clause that cut reviewer candidate-finding rate ~8x in production — `pr_migration_test.go` now guards against any skill re-introducing a suppression instruction.
- `domain-boundary` — review only your declared domain; PASS when it is clean. (The shared stay-in-lane + zero-concerns-PASS contract.)
