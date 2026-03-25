# Rick Architecture

## System Overview

```mermaid
graph TB
    subgraph External["External Input"]
        CLI["CLI / MCP Client"]
        Agent["rick-agent<br/>(Wails + Svelte + Gemini ADK)"]
    end

    subgraph Core["Core Infrastructure"]
        Store[("SQLite Event Store<br/>(WAL, optimistic concurrency)<br/>Tag-based lookup")]
        Bus["Event Bus<br/>(ChannelBus / OutboxBus)"]
    end

    subgraph Engine["Engine — Lifecycle Only"]
        Agg["WorkflowAggregate<br/>Apply() + Decide()"]
        WDef["WorkflowDef<br/>{Required, MaxIter}"]
    end

    subgraph Runner["PersonaRunner — Sole Dispatcher"]
        Trigger["Trigger Check<br/>Events + AfterPersonas"]
        Safety["Safety Guards<br/>dedup · chain depth · width limit<br/>join-gate dedup · dispatch queue<br/>self-trigger prevention"]
    end

    subgraph Handlers["Persona Handlers"]
        R["researcher"]
        A["architect"]
        D["developer"]
        REV["reviewer"]
        QA["qa"]
        DOC["documenter"]
        COM["committer"]
    end

    subgraph External_Handlers["External Handlers (gRPC)"]
        EH1["frontend-enricher"]
        EH2["security-scanner"]
        EH3["component-catalog"]
    end

    subgraph Backend["LLM Backend"]
        Claude["Claude CLI"]
        Gemini["Gemini CLI"]
    end

    subgraph Projections["Read Models"]
        P1["WorkflowStatus"]
        P2["TokenUsage"]
        P3["PhaseTimeline"]
        P4["Verdict"]
    end

    CLI -->|WorkflowRequested| Store
    CLI -->|Publish| Bus
    Agent -->|MCP HTTP| CLI

    Bus -->|lifecycle events| Engine
    Bus -->|persona events| Runner
    Bus -->|all events| Projections

    Engine --> Store
    Engine -->|WorkflowStarted<br/>WorkflowCompleted<br/>FeedbackGenerated| Bus

    Runner --> Trigger
    Trigger --> Safety
    Safety -->|dispatch| Handlers
    Safety -->|gRPC stream| External_Handlers
    Handlers -->|PersonaCompleted<br/>PersonaFailed| Bus
    External_Handlers -->|HandlerResult| Bus
    Handlers --> Store

    Handlers --> Backend
    Backend --> Claude
    Backend --> Gemini

    Store --> Projections
```

## Event Flow — Default Workflow Example

**User prompt:** `"Build a REST API for user management"`

```mermaid
sequenceDiagram
    participant User as CLI / MCP / rick-agent
    participant Bus as Event Bus
    participant Engine as Engine
    participant PR as PersonaRunner
    participant Store as Event Store
    participant R as researcher
    participant A as architect
    participant D as developer
    participant REV as reviewer
    participant QA as qa
    participant DOC as documenter
    participant COM as committer
    participant LLM as Claude/Gemini

    Note over User,LLM: Phase 1: Workflow Initialization

    User->>Store: Append(WorkflowRequested)
    User->>Bus: Publish(WorkflowRequested)
    Bus->>Engine: WorkflowRequested
    Engine->>Store: Load aggregate → Decide()
    Engine->>Store: Append(WorkflowStarted)
    Engine->>Bus: Publish(WorkflowStarted)

    Note over User,LLM: Phase 2: Sequential Chain (researcher → architect → developer)

    Bus->>PR: WorkflowStarted
    PR->>PR: Trigger check: researcher subscribes to WorkflowStarted ✓
    PR->>R: dispatch
    R->>LLM: "Research frameworks, patterns, prior art..."
    LLM-->>R: analysis output
    R-->>PR: [AIRequestSent, AIResponseReceived]
    PR->>Store: Append(PersonaCompleted{researcher})
    PR->>Bus: Publish(PersonaCompleted{researcher})

    Bus->>PR: PersonaCompleted{researcher}
    PR->>PR: Trigger check: architect, after:[researcher] ✓
    PR->>Store: LoadByCorrelation → researcher completed ✓
    PR->>A: dispatch
    A->>LLM: "Design API structure, schemas, endpoints..."
    LLM-->>A: architecture plan
    A-->>PR: [AIRequestSent, AIResponseReceived]
    PR->>Store: Append(PersonaCompleted{architect})
    PR->>Bus: Publish(PersonaCompleted{architect})

    Bus->>PR: PersonaCompleted{architect}
    PR->>PR: Trigger check: developer, after:[architect] ✓
    PR->>D: dispatch
    D->>LLM: "Implement the REST API based on architect's plan..."
    LLM-->>D: implementation code
    D-->>PR: [AIRequestSent, AIResponseReceived]
    PR->>Store: Append(PersonaCompleted{developer})
    PR->>Bus: Publish(PersonaCompleted{developer})

    Note over User,LLM: Phase 3: Parallel Fan-Out (reviewer + qa + documenter)

    par reviewer fires
        Bus->>PR: PersonaCompleted{developer}
        PR->>PR: Trigger: reviewer, after:[developer] ✓
        PR->>REV: dispatch
        REV->>LLM: "Review code for correctness, security..."
        LLM-->>REV: "VERDICT: FAIL — missing error handling"
        REV-->>PR: [AIRequestSent, AIResponseReceived, VerdictRendered{fail}]
        PR->>Store: Append(PersonaCompleted{reviewer})
        PR->>Bus: Publish(PersonaCompleted{reviewer})
    and qa fires
        Bus->>PR: PersonaCompleted{developer}
        PR->>PR: Trigger: qa, after:[developer] ✓
        PR->>QA: dispatch
        QA->>LLM: "Verify test coverage, edge cases..."
        LLM-->>QA: "VERDICT: PASS"
        QA-->>PR: [AIRequestSent, AIResponseReceived, VerdictRendered{pass}]
        PR->>Store: Append(PersonaCompleted{qa})
        PR->>Bus: Publish(PersonaCompleted{qa})
    and documenter fires
        Bus->>PR: PersonaCompleted{developer}
        PR->>PR: Trigger: documenter, after:[developer] ✓
        PR->>DOC: dispatch
        DOC->>LLM: "Generate API docs, README..."
        LLM-->>DOC: documentation
        DOC-->>PR: [AIRequestSent, AIResponseReceived]
        PR->>Store: Append(PersonaCompleted{documenter})
        PR->>Bus: Publish(PersonaCompleted{documenter})
    end

    Note over User,LLM: Phase 4: Feedback Loop (reviewer said FAIL)

    Bus->>Engine: VerdictRendered{fail, phase=developer, source=reviewer}
    Engine->>Store: Load aggregate → Decide()
    Note over Engine: iteration 1 < maxIter(3) → emit feedback
    Engine->>Store: Append(FeedbackGenerated{target=developer})
    Engine->>Bus: Publish(FeedbackGenerated)
    Note over Engine: Clears CompletedPersonas[developer, reviewer]

    Bus->>PR: FeedbackGenerated
    PR->>PR: Trigger: developer subscribes to FeedbackGenerated ✓
    PR->>D: dispatch (retry with feedback context)
    D->>LLM: "Fix: missing error handling in handler.go:42..."
    LLM-->>D: fixed implementation
    D-->>PR: [AIRequestSent, AIResponseReceived]
    PR->>Store: Append(PersonaCompleted{developer})
    PR->>Bus: Publish(PersonaCompleted{developer})

    Note over User,LLM: Phase 5: Parallel Re-Fire (all downstream re-trigger)

    par reviewer re-fires
        Bus->>PR: PersonaCompleted{developer}
        PR->>REV: dispatch
        REV->>LLM: "Re-review fixed code..."
        LLM-->>REV: "VERDICT: PASS"
        REV-->>PR: [AIRequestSent, AIResponseReceived, VerdictRendered{pass}]
        PR->>Bus: Publish(PersonaCompleted{reviewer})
    and qa re-fires
        Bus->>PR: PersonaCompleted{developer}
        PR->>QA: dispatch
        QA->>LLM: "Re-verify..."
        LLM-->>QA: "VERDICT: PASS"
        PR->>Bus: Publish(PersonaCompleted{qa})
    and documenter re-fires
        Bus->>PR: PersonaCompleted{developer}
        PR->>DOC: dispatch
        PR->>Bus: Publish(PersonaCompleted{documenter})
    end

    Note over User,LLM: Phase 6: Join Gate → Commit → Complete

    Bus->>PR: PersonaCompleted{reviewer}
    PR->>PR: Trigger: committer, after:[reviewer, qa]
    PR->>Store: LoadByCorrelation → reviewer ✓, qa ✓
    PR->>PR: Join-gate dedup: fingerprint(reviewerID|qaID) → new ✓
    PR->>COM: dispatch
    COM->>LLM: "Commit changes, create PR..."
    LLM-->>COM: commit result
    PR->>Store: Append(PersonaCompleted{committer})
    PR->>Bus: Publish(PersonaCompleted{committer})

    Bus->>Engine: PersonaCompleted{committer}
    Engine->>Store: Load aggregate
    Note over Engine: All Required completed:<br/>researcher ✓ architect ✓ developer ✓<br/>reviewer ✓ qa ✓ committer ✓
    Engine->>Store: Append(WorkflowCompleted)
    Engine->>Bus: Publish(WorkflowCompleted)

    Bus-->>User: WorkflowCompleted 🎉
```

## Trigger Configuration (All Handlers)

```mermaid
graph LR
    subgraph "Event Subscriptions"
        WS["WorkflowStarted"]
        PC["PersonaCompleted"]
        FG["FeedbackGenerated"]
    end

    subgraph "Handlers + Join Gates"
        R["researcher<br/>after: —"]
        A["architect<br/>after: researcher"]
        D["developer<br/>after: architect"]
        REV["reviewer<br/>after: developer"]
        QA["qa<br/>after: developer"]
        DOC["documenter<br/>after: developer"]
        COM["committer<br/>after: reviewer + qa"]
    end

    WS --> R
    PC --> A
    PC --> D
    FG --> D
    PC --> REV
    PC --> QA
    PC --> DOC
    PC --> COM

    style R fill:#4CAF50,color:#fff
    style A fill:#2196F3,color:#fff
    style D fill:#FF9800,color:#fff
    style REV fill:#E91E63,color:#fff
    style QA fill:#E91E63,color:#fff
    style DOC fill:#9C27B0,color:#fff
    style COM fill:#795548,color:#fff
```

## Feedback Loop State Machine

```mermaid
stateDiagram-v2
    [*] --> Requested: WorkflowRequested
    Requested --> Running: WorkflowStarted

    Running --> Running: PersonaCompleted (partial)
    Running --> Completed: PersonaCompleted (all required done)
    Running --> Failed: PersonaFailed
    Running --> Failed: TokenBudgetExceeded

    Running --> Running: VerdictRendered{fail} → FeedbackGenerated<br/>(iteration < maxIter)
    Running --> Failed: VerdictRendered{fail}<br/>(iteration >= maxIter, escalate=false)
    Running --> Paused: VerdictRendered{fail}<br/>(iteration >= maxIter, escalate=true)

    Running --> Paused: WorkflowPaused
    Paused --> Running: WorkflowResumed
    Running --> Cancelled: WorkflowCancelled
    Paused --> Cancelled: WorkflowCancelled

    Completed --> [*]
    Failed --> [*]
    Cancelled --> [*]
```

**Resume-after-escalation:** When a paused workflow is resumed (WorkflowResumed), the Engine detects the phase that hit MaxIterations, bumps the limit by 1, and re-emits FeedbackGenerated. This triggers the developer to re-run with operator guidance visible in the correlation chain.

## Component Responsibilities

```
┌─────────────────────────────────────────────────────────────────┐
│                  CLI / MCP / rick-agent                          │
│  Accepts user prompt → creates WorkflowRequested event          │
│  rick-agent: Wails desktop app → Gemini ADK → MCP tools         │
└──────────────────────────┬──────────────────────────────────────┘
                           │
┌──────────────────────────▼──────────────────────────────────────┐
│                      Event Bus                                  │
│  Pub/Sub with middleware (logging, metrics, dead letter, retry)  │
│  ChannelBus (in-process) · OutboxBus (transactional outbox)     │
└───┬──────────────┬──────────────┬───────────────────────────────┘
    │              │              │
    ▼              ▼              ▼
┌────────┐  ┌───────────┐  ┌──────────────┐
│ Engine │  │ Persona   │  │ Projections  │
│        │  │ Runner    │  │              │
│ WHAT:  │  │ WHAT:     │  │ WHAT:        │
│ Life-  │  │ Dispatch  │  │ Read models  │
│ cycle  │  │ handlers  │  │ for queries  │
│ only   │  │           │  │              │
│        │  │ HOW:      │  │ HOW:         │
│ HOW:   │  │ Trigger   │  │ Catch-up +   │
│ Aggre- │  │ matching, │  │ live sub     │
│ gate   │  │ join gate │  │              │
│ Decide │  │ check,    │  └──────────────┘
│        │  │ safety    │
│ EMITS: │  │ guards    │
│ Started│  │           │
│ Done   │  │ EMITS:    │
│ Failed │  │ Persona   │
│ Feed-  │  │ Completed │
│ back   │  │ Persona   │
└────────┘  │ Failed    │
            └─────┬─────┘
                  │
    ┌─────────────┼─────────────┐
    ▼             ▼             ▼
┌────────┐  ┌────────┐   ┌──────────┐
│Handler │  │Handler │   │ Backend  │
│Registry│  │  impl  │   │ (Claude/ │
│        │  │ (AI +  │   │  Gemini) │
│ Name→H │  │ prompt │   │          │
│ lookup │  │ build) │   │ CLI sub- │
└────────┘  └────────┘   │ process  │
                          └──────────┘

┌─────────────────────────────────────────────────────────────────┐
│                    SQLite Event Store                            │
│  Append-only · Optimistic concurrency · WAL mode                │
│  LoadByCorrelation (cross-aggregate join queries)               │
│  LoadByTag (business key → correlation lookup)                  │
│  Snapshots · Dead letter queue                                  │
└─────────────────────────────────────────────────────────────────┘
```

## Workflow Definitions

### Built-in Workflows

| Workflow | Required Personas | MaxIter | Escalate |
|----------|-------------------|---------|----------|
| `default` | researcher, architect, developer, reviewer, qa, committer | 3 | no |
| `develop-only` | developer, reviewer | 3 | no |
| `workspace-dev` | workspace, context-snapshot, developer, reviewer, qa, committer | 3 | yes |
| `pr-review` | pr-workspace, pr-jira-context, pr-architect, pr-reviewer, pr-qa, pr-consolidator, pr-cleanup | 1 | no |
| `pr-feedback` | workspace, feedback-analyzer, context-snapshot, developer, reviewer, committer | 3 | yes |
| `jira-dev` | jira-context, researcher, architect, developer, reviewer, qa, committer | 3 | yes |
| `ci-fix` | developer, reviewer, qa, committer | 2 | yes |
| `plan-btu` | confluence-reader, codebase-researcher, plan-architect, estimator, confluence-writer | 3 | yes |

### Plugin-Provided Workflows (dynamically registered via gRPC)

| Workflow | Required Personas | Plugin | MaxIter |
|----------|-------------------|--------|---------|
| `plan-jira` | page-reader, project-manager, jira-task-creator | rick-jira-planner | 3 |
| `task-creator` | task-creator | rick-jira-planner | 1 |

## Before-Hooks (External Enrichment)

External systems can insert into the handler chain without modifying handler code. Hooks inject additional join conditions at runtime via `WithBeforeHook`.

```
Without hook:  architect → developer
With hook:     architect → frontend-enricher → developer

developer's declared AfterPersonas: ["architect"]
+ hook injection:                   ["frontend-enricher"]
= effective join:                   ["architect", "frontend-enricher"]
```

## Dispatch Queue (Priority Ordering)

Each handler gets a per-correlation priority queue. Events for the same (handler, workflow) are serialized — no concurrent execution. When multiple events are pending, highest priority processes first.

| Priority | Event Type | Value |
|----------|-----------|-------|
| Highest | `OperatorGuidance` | 0 |
| High | `FeedbackGenerated` | 10 |
| Normal | `PersonaCompleted` / `PersonaFailed` | 20 |
| Default | `WorkflowStarted`, others | 30 |

## Tag-Based Correlation Lookup

The Engine auto-indexes business keys from `WorkflowRequested` as tags. External systems discover workflows by business identifier instead of UUID.

```go
// Discover workflow by Jira ticket
ids, _ := store.LoadByTag(ctx, "ticket", "PROJ-123")    // → ["corr-abc"]

// Discover by repo+branch
ids, _ := store.LoadByTag(ctx, "repo_branch", "org/repo:main")

// Discover by source
ids, _ := store.LoadByTag(ctx, "source", "jira:PROJ-123")
```

| Tag Key | Extracted From | Example Value |
|---------|---------------|---------------|
| `source` | `WorkflowRequested.Source` | `"jira:PROJ-123"` |
| `ticket` | `WorkflowRequested.Ticket` | `"PROJ-123"` |
| `repo` | `WorkflowRequested.Repo` | `"acme/myapp"` |
| `repo_branch` | `Repo:BaseBranch` | `"acme/myapp:main"` |
| `workflow_id` | `WorkflowRequested.WorkflowID` | `"default"` |

## gRPC Service Discovery

External systems connect via bidirectional gRPC streams. The stream lifecycle IS the registration — no separate service registry needed.

```
External System                         Rick (PersonaService)
     │                                        │
     │── HandleStream (open) ────────────────>│
     │   HandlerRegistration{                 │
     │     name: "frontend-enricher"          │
     │     events: ["persona.completed"]      │  Creates proxy handler
     │     after: ["architect"]               │  Wires bus subscriptions
     │     hooks: ["developer"]               │  Registers before-hook
     │   }                                    │
     │<── RegistrationAck{ok} ───────────────│
     │                                        │
     │   ... architect completes ...          │
     │                                        │
     │<── DispatchRequest{event} ────────────│  PersonaRunner trigger + join ✓
     │                                        │
     │   ... process event ...                │
     │                                        │
     │── HandlerResult{events} ──────────────>│  PersonaCompleted emitted
     │                                        │
     │── stream close ───────────────────────>│  Auto-deregistration
```

### Dispatch routing

```
CompositeDispatcher
  ├── LocalDispatcher    → built-in personas (in-process, <1ms)
  └── StreamDispatcher   → external handlers (gRPC stream, ~10-50ms)
       ├── "frontend-enricher" → Python service
       └── "security-scanner"  → Go microservice
```

### Proto contract

```protobuf
service PersonaService {
  rpc HandleStream(stream HandlerMessage) returns (stream DispatchMessage);
}

message HandlerRegistration {
  string name = 1;
  repeated string event_types = 2;        // what wakes this handler
  repeated string after_personas = 3;      // join condition
  repeated string before_hook_targets = 4; // personas to gate
}
```

### Reconnecting client

`Client.Run(ctx)` (`internal/grpchandler/client.go`) provides production-grade stream resilience:

```go
client := grpchandler.NewClient(conn, grpchandler.ClientConfig{
    Name:          "frontend-enricher",
    EventTypes:    []string{"persona.completed"},
    AfterPersonas: []string{"architect"},
    Handler:       myHandlerFunc,
})
go client.Run(ctx) // reconnects automatically until ctx cancelled
```

- Exponential backoff: BaseDelay(1s) x 2^attempt, capped at MaxDelay(30s)
- Automatic re-registration on each reconnect
- Configurable MaxRetries (0 = unlimited)

Safety guards (self-trigger, chain depth, dedup, join conditions, priority queue) all remain in PersonaRunner. External handlers are pure event processors — Rick owns all choreography logic.
