# skills (`skills/`)

Reusable skill fragments composed into persona system prompts. One subdir per skill (`SKILL.md`: YAML frontmatter + markdown body). A persona's manifest lists `skills:` in order; the resolver appends each skill's body after the identity.

## Format
- Frontmatter: `name`, `description`, optional `tools[]` (MCP tool allowlist, Claude only). Parsed by `../manifest.go` (`ParseSkillManifest`).
- Body: the skill instruction text (required).
- Same forbidden-key safety boundary as persona manifests.

## Current skills
- `diff-grounding` — every finding cites the exact changed file/line; reject ungrounded claims. (The shared reviewer citation rule, deduped here from the 13 `pr-*` prompts.)
- `domain-boundary` — review only your declared domain; PASS when it is clean. (The shared stay-in-lane + zero-concerns-PASS contract.)
