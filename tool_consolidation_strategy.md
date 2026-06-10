# Tool Consolidation Strategy

## 1. Problem Statement
The `rick` MCP server currently exposes **~45 separate tools** to its agents. Tools are heavily fragmented (e.g., 12 separate Jira tools, 16 separate workflow tools). This causes context saturation, slows down the agent's tool-selection process, and inevitably leads to choice paralysis or hallucinated tool names.

## 2. Constraints
* **Context Saturation:** We must drastically reduce the raw count of tool definitions presented to the LLM.
* **Functionality:** No underlying capabilities should be lost. 
* **Telemetry Data:** While I searched `rick.db` and the codebase for explicit tool-usage telemetry (like hit-rates or error rates), the structural overlap in the tool definitions is glaring enough to simplify immediately without needing runtime metrics to justify it.

## 3. Proposed Solution (The "Happy Path")
We need to shift from a "RPC per endpoint" design to a "GraphQL-like / Facade" design for the tools. I propose consolidating the ~45 tools down to **~15 essentials**:

### A. Jira Consolidation (12 tools → 3 tools) (Done)
*   **`rick_jira_read`**: Enhanced to accept flags for joining `qa_steps`, `pr_links`, and `epic_issues`. (Eliminates `rick_jira_read_qa_steps`, `rick_jira_pr_links`, `rick_jira_epic_issues`).
*   **`rick_jira_write`**: Expanded schema to handle comments, transitions, and custom fields like microservices. (Eliminates `rick_jira_comment`, `rick_jira_transition`, `rick_jira_set_microservice`).
*   **`rick_jira_manage_links`**: Merged creation and deletion of links into a single tool.

### B. Workflow & Observability Consolidation (23 tools → ~5 tools) (Done)
*   **`rick_workflow_inspect`**: Merge `rick_workflow_status`, `rick_phase_timeline`, `rick_token_usage`, `rick_list_events`, `rick_workflow_verdicts`, and `rick_workflow_output`. Pass an array of `include: ["status", "timeline", "tokens", "output"]` to fetch exactly what is needed in one shot. (Eliminates `rick_persona_output` which is entirely redundant).
*   **`rick_workflow_control`**: Merge `rick_pause_workflow`, `rick_resume_workflow`, and `rick_cancel_workflow` into one tool with an `action` enum.
*   **`rick_diff_viewer`**: Merge `rick_diff` and `rick_pr_diff` into a single tool that routes based on whether a `workflow_id` or `repo`/`pr_number` is provided.

### C. Job & Wave Tools (Done)
*   **`rick_job_inspect`**: Merges `rick_job_status` and `rick_job_output` (status + output via an `include` list; output supports incremental `offset` reads).
*   **`rick_wave_manager`**: Merges wave `plan` / `launch` / `status` / `cleanup` behind a single `action` enum. `rick_github_pr_links` stays standalone — it is not a wave lifecycle verb.

## 5. Outcome
Tool surface reduced from ~45 to **33** across 7 categories: workflow (10), jobs (6), workspace (3), jira (5), wave (2), observability (5), confluence (2). Facade tools (`tools_consolidated.go`) dispatch to the original handlers, which are retained unchanged — no capability lost. Consumers migrated: the `agent/` Wails dashboard (`dashboard.go`), the operator LLM tool catalog (`operator.go`), README, and the package CLAUDE.md docs. Schemas kept flat with simple string enums (`include`, `action`) per the kill-shot mitigation.

## 4. The "Kill Shot" (Risk Assessment)
The most likely reason this design could fail is that **consolidated tools require more complex input schemas**. If we make the `rick_workflow_inspect` schema too nested, the LLM will hallucinate arguments or fail to format the JSON correctly. We must keep the schemas flat and use simple string enums for multiplexing. Also, tests that rely on the old names of the tools will need to be refactored, which is currently ongoing for the workflow consolidation tools.
