# Rick Agent Team

A committed, project-scoped team of Claude Code subagents for this repo, living in
`.claude/agents/` and shared with everyone who clones it. Each is a specialist: four cover
the **plan → develop → review → qa** lifecycle (deliberately mirroring how Rick itself splits
personas), and five are cross-cutting advisors you pull in as needed.

## Lifecycle crew

| Agent | Stage | Tools | Model | Owns |
|---|---|---|---|---|
| `planner` | Design | read-only | opus | Per-change Design Brief: event/DAG impact, blast radius, rollout/rollback |
| `developer` | Build | read + edit/write | inherit | Implementation following repo conventions; runs `make check` |
| `reviewer` | Review | read-only | opus | Code-as-written: correctness, concurrency, data integrity, API stability |
| `qa` | Test | read + edit/write | sonnet | Ship-readiness: coverage, flakiness, race, rollback |

## Advisors & specialists

| Agent | Role | Tools | Model | Owns |
|---|---|---|---|---|
| `architect` | System design | read-only | opus | Big-picture shape: where code belongs, state ownership, topology, back-compat strategy |
| `opportunity-scout` | Optimist | read-only | opus | Opportunities — simplifications, leverage, capability gaps, well-fit tech (value vs effort) |
| `skeptic` | Pessimist | read-only | opus | Adversarial critique of proposals — risks, failure modes, the kill shot |
| `dx-expert` | Dev experience | read + edit/write | sonnet | Ergonomics, error quality, naming, workflow friction, CLAUDE.md/doc usability |
| `mcp-expert` | MCP subsystem | read + edit/write | opus | `internal/mcp/` — JSON-RPC compliance, transport, protocol negotiation, the 18-tool facade |

`opportunity-scout` ↔ `skeptic` are an intentional optimist/pessimist pair — run both on a
proposal to get balanced input. `architect` decides *whether a direction is right*; `planner`
plans *how to build the agreed thing*. `reviewer` audits written code; `skeptic` challenges
ideas before they're built.

## How to use them

**Let Claude auto-delegate** — phrase the request so the right specialist is obvious:

```
Is the DAG the right place for dead-letter retry, or does it belong in the engine?   # -> architect
Plan the change to add a dead-letter retry handler                                   # -> planner
Implement the plan above                                                             # -> developer
Review this diff for concurrency bugs                                                # -> reviewer
Check ship-readiness and test coverage for this change                               # -> qa
What's the riskiest part of this proposal?                                           # -> skeptic
Where could we get more leverage out of the event log?                               # -> opportunity-scout
Are these error messages and names pulling their weight?                             # -> dx-expert
Should this new capability be a new rick_* tool or fold into a facade?               # -> mcp-expert
```

**Or invoke explicitly** by name:

```
@agent-reviewer review the PersonaRunner priority-queue change
@agent-mcp-expert audit the new tool against the TestToolsList count guard
```

**Run agents in parallel** (they're independent) by asking for several in one message:

```
Have the reviewer and qa agents both vet the current diff.
Get the opportunity-scout and skeptic to weigh in on this design — optimist and pessimist.
```

## As experimental agent teammates

These same definitions double as teammate templates for the experimental Agent Teams feature
(multiple independent sessions that message each other). Enable and spawn:

```bash
export CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1
claude
```
```
Spawn a teammate using the reviewer agent type and one using the qa agent type;
have them review the current branch in parallel and report findings.
```

Agent teams cost far more tokens (each teammate is a full Claude instance) — use them only when
you need true parallel work or inter-agent debate, not for everyday delegation.

## Maintaining the team

- Edit definitions via `/agents` (no restart needed) or edit the `.md` files directly (restart
  the session to reload).
- Keep `name` fields unique within `.claude/agents/`.
- Tune `model`/`tools` per agent as cost/scope needs change — `model: inherit` follows your
  session model; read-only agents cannot edit files by design.
- `.gitignore` shares only `.claude/agents/` and `.claude/TEAM.md`; the rest of `.claude/`
  (local commands, worktrees, session state) stays local.
- Reference: https://code.claude.com/docs/en/sub-agents.md
