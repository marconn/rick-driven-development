# persona manifests (`personas/`)

Data-driven persona manifests (`SKILL.md`: YAML frontmatter + markdown identity body). One subdir per persona; the 13 `pr-*` category reviewers were migrated here in epic task 0009.

## Format
- Frontmatter: `schema_version` (1), `name`, `description`, optional `skills[]` (composed in order), optional `knowledge[]{pack, load, criticality}`. Parsed + validated by `../manifest.go`.
- Body: the identity system prompt (the reviewer's `## Your Domain`, `## Severity Guide`, and domain-specific rules). Shared boilerplate is referenced via `skills`, not inlined.
- **Forbidden:** safety/runtime keys (`yolo`, `plaintext`, `verdict_bearing`, `backend`, `timeout`, `target`, `phase`, `effort`). Rejected loudly at load — these stay code-owned in `handler/handlers.go`.

## Loading
Resolved by `../resolver.go` (`LoadManifestDir`) and composed by `ManifestSource.ComposeSystemPrompt` = identity + ordered skill fragments. Opt-in via `RICK_PERSONA_MANIFESTS_DIR`; unset ⇒ the embedded `../prompts/<name>.md` are used unchanged (the rollback fallback, kept intact per F15). Manifest WINS on name collision.

The equivalence net (`../pr_migration_test.go`) asserts each migrated reviewer's composed prompt is a superset of its embedded prompt's domain rules.
