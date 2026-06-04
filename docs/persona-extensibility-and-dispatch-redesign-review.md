# Review Critique: Persona Extensibility & Dispatch Stability Proposal

Source: `docs/persona-extensibility-and-dispatch-redesign.md`

## Findings

### High: Track A overclaims runtime extensibility

The proposal says manifest personas let operators "drop a dir, restart" to add
or extend personas. That is true only for prompt composition on existing AI
handlers. A new runnable workflow participant still needs static handler
construction, handler registry registration, and workflow DAG membership.

Current hardcoded points:

- `internal/handler/handlers.go`: `RegisterAll` constructs the known handlers.
- `internal/cli/run.go`: `selectWorkflowDef` maps names to built-in workflow
  definitions.
- `internal/cli/serve.go`: startup registers a fixed list of workflow
  definitions.

The design should explicitly distinguish:

- Manifest override/composition for existing handlers.
- Dynamic creation of new AI handlers.
- Dynamic workflow topology changes.

The current proposal only fully specifies the first.

### High: Track B's projection is underspecified as a readiness source of truth

The proposal names a `WorkflowRuntimeState` projection with
`pending -> ready -> running -> completed / failed / stale`, but the current
event stream does not durably record all of those transitions.

Completion and failure are durable through `PersonaCompleted` and
`PersonaFailed`. AI handlers also emit `AIRequestSent` and `AIRequestStarted`,
but non-AI handlers do not have a general `PersonaStarted` or
`DispatchStarted` event. Queue admission, readiness, blocked-on-pause, hint-only
execution, `ErrIncomplete`, stale entry, and stale exit are primarily runner
runtime facts today.

Without a new event contract, the projection either:

- Re-implements the legacy replay logic it is meant to replace.
- Cannot model the states it claims to expose.

The proposal should define the durable events or explicit in-memory transition
contract required by the projection before treating Track B as implementation
ready.

### Medium: Rollout defaults contradict rollback guarantees

The rollout table says `RICK_DISPATCH_PROJECTION=shadow|active|off` defaults to
`shadow`, but the section also says every flag defaults to current behavior.
Shadow mode is not current behavior if it computes projection readiness and
emits `JoinDivergence` diagnostics.

Recommended correction:

- Default to `off` for strict current behavior, or
- State clearly that `shadow` is an intentional additive default and define its
  resource and diagnostic impact.

### Medium: Knowledge semantics conflict with retained multi-backend support

The proposal keeps multi-backend support as a retained requirement, but Phase 1
makes knowledge packs available only on Claude via MCP. Non-Claude backends run
with the knowledge layer unavailable and only emit a diagnostic event.

That weakens the core formula:

`operation mode = identity x skills x knowledge`

For review rotations or non-Claude default deployments, operation mode becomes
backend-dependent. The proposal needs a policy for declared knowledge:

- `required`: fail dispatch or pin/select a backend that can provide it.
- `optional`: run degraded and emit `knowledge_unavailable`.
- `always_inline`: allowed only when token budgets and pack size pass validation.

Without that distinction, operators can believe they changed a persona's
operating mode while most configured backends silently ignore the knowledge
layer except for telemetry.

### Medium: Manifest schema mixes persona identity with handler safety knobs

The illustrative manifest includes `runtime.effort`, `runtime.verdict_bearing`,
and `phase`. Today several safety-critical behaviors are configured in handler
construction rather than persona prompt loading:

- `PlainText=true` for verdict-bearing reviewers.
- `Yolo=false` for `pr-replier`.
- Review backend selection and review timeout.
- Target persona for review handlers.
- Template mapping from handler name to phase prompt.
- Per-persona effort.

The proposal should define precedence and validation rules for these fields.
Otherwise an operator manifest could accidentally bypass safety behavior that is
currently enforced in Go.

Recommended framing:

- Persona manifests own prompt composition.
- Handler manifests, if introduced, own runtime/safety behavior.
- Workflow manifests, if introduced, own DAG topology.

Keeping those contracts separate preserves the proposal's Track A / Track B
decoupling.

### Low: Agent SDK rejection should be refreshed and narrowed

The rejection is mostly defensible: current official Claude Agent SDK
documentation lists Python and TypeScript SDKs, not Go, and using it would be a
Claude-specific runtime path.

However, the subagent argument is overstated. Current docs describe subagents,
MCP, skills, hooks, sessions, and dynamic workflow material. The stronger
argument is not "the SDK has no subagents"; it is:

- The SDK is Claude-specific.
- There is no official Go Agent SDK.
- Adopting it would introduce a Python or TypeScript runtime into a single Go
  binary deployment.
- Rick would still need to preserve its event-sourced workflow lifecycle,
  durable DAG joins, feedback invalidation, retry, recovery, and multi-backend
  adapter semantics.

References:

- https://code.claude.com/docs/en/agent-sdk/overview
- https://code.claude.com/docs/en/agent-sdk/subagents

## Overall Assessment

The proposal's strongest idea is the enforced separation between persona
extensibility and dispatch stability. That boundary should stay.

The main change needed before sign-off is precision:

- Narrow Track A to prompt composition unless dynamic handler/workflow manifests
  are explicitly added.
- Make Track B's state transition and event contract concrete.
- Define how required knowledge behaves across non-Claude backends.
- Split persona identity fields from handler/runtime safety fields.

With those corrections, this becomes a much more reviewable and implementable
design.
