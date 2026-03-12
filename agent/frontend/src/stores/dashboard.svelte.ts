// Dashboard store — workflow list, detail, phases, tokens, dead letters.
// Follows the same module-level $state pattern as chat.svelte.ts.

export interface WorkflowSummary {
  aggregate_id: string
  workflow_id: string
  status: string
  fail_reason?: string
  started_at?: string
  completed_at?: string
}

export interface WorkflowDetail {
  id: string
  status: string
  workflow_id: string
  version: number
  tokens_used: number
  completed_personas: Record<string, boolean>
  feedback_count: Record<string, number>
  pending_hints?: PendingHint[]
}

export interface PhaseEntry {
  phase: string
  status: string
  iterations: number
  started_at?: string
  completed_at?: string
  duration_ms?: number
}

export interface TokenUsage {
  workflow_id: string
  total: number
  by_phase: Record<string, number>
  by_backend: Record<string, number>
}

export interface DeadLetterEntry {
  id: string
  event_id: string
  handler: string
  error: string
  attempts: number
  failed_at: string
}

export interface PendingHint {
  persona: string
  confidence: number
  plan: string
  blockers?: string[]
  token_estimate: number
  event_id: string
}

export interface ActionResult {
  workflow_id: string
  action: string
  status: string
  resumed?: boolean
}

export interface VerdictIssue {
  severity: string
  category: string
  description: string
  file?: string
  line?: number
}

export interface VerdictRecord {
  phase: string
  source_phase: string
  outcome: string
  summary: string
  issues?: VerdictIssue[]
}

export interface PersonaOutput {
  workflow_id: string
  persona: string
  output: string
  truncated: boolean
  backend: string
  tokens_used: number
  duration_ms: number
}

let workflows = $state<WorkflowSummary[]>([])
let selectedWorkflowId = $state<string | null>(null)
let workflowDetail = $state<WorkflowDetail | null>(null)
let phases = $state<PhaseEntry[]>([])
let tokens = $state<TokenUsage | null>(null)
let deadLetterCount = $state(0)
let serverReachable = $state(true)
let error = $state<string | null>(null)
let verdicts = $state<VerdictRecord[]>([])
let personaOutputs = $state<Record<string, PersonaOutput>>({})
let outputModalPersona = $state<string | null>(null)

let pollTimer: ReturnType<typeof setInterval> | null = null
let detailTimer: ReturnType<typeof setInterval> | null = null

let activeCount = $derived(workflows.filter(w => w.status === 'running').length)
let failedCount = $derived(workflows.filter(w => w.status === 'failed').length)
let pausedCount = $derived(workflows.filter(w => w.status === 'paused').length)

// Sort: paused (approval-pending) first, then running, then the rest.
// Tiebreak by aggregate_id for stable ordering across poll cycles.
let sortedWorkflows = $derived(
  [...workflows].sort((a, b) => {
    const order: Record<string, number> = { paused: 0, running: 1, failed: 2, cancelled: 3, completed: 4 }
    const d = (order[a.status] ?? 5) - (order[b.status] ?? 5)
    if (d !== 0) return d
    return a.aggregate_id < b.aggregate_id ? -1 : a.aggregate_id > b.aggregate_id ? 1 : 0
  })
)

async function fetchWorkflows() {
  try {
    // @ts-ignore — Wails runtime
    if (!window.go?.main?.App?.ListWorkflows) return
    // @ts-ignore
    const result = await window.go.main.App.ListWorkflows()
    workflows = result ?? []
    serverReachable = true
    error = null
  } catch (e) {
    serverReachable = false
    error = e instanceof Error ? e.message : String(e)
  }
}

async function fetchDeadLetters() {
  try {
    // @ts-ignore
    if (!window.go?.main?.App?.ListDeadLetters) return
    // @ts-ignore
    const result = await window.go.main.App.ListDeadLetters()
    deadLetterCount = result?.length ?? 0
  } catch {
    // Non-critical — don't set serverReachable to false
  }
}

async function fetchDetail(id: string) {
  try {
    // @ts-ignore
    const [detail, timeline, usage, vList] = await Promise.all([
      window.go.main.App.WorkflowStatus(id),
      window.go.main.App.PhaseTimeline(id),
      window.go.main.App.TokenUsageForWorkflow(id),
      window.go.main.App.WorkflowVerdicts(id),
    ])
    workflowDetail = detail
    phases = timeline ?? []
    tokens = usage
    verdicts = vList ?? []
  } catch (e) {
    error = e instanceof Error ? e.message : String(e)
  }
}

async function fetchPersonaOutput(workflowID: string, persona: string): Promise<PersonaOutput | null> {
  try {
    // @ts-ignore
    const result = await window.go.main.App.PersonaOutput(workflowID, persona)
    if (result) {
      personaOutputs = { ...personaOutputs, [persona]: result }
      outputModalPersona = persona
    }
    return result
  } catch (e) {
    error = e instanceof Error ? e.message : String(e)
    return null
  }
}

function closeOutputModal() {
  outputModalPersona = null
}

function startPolling(scope: 'full' | 'minimal') {
  stopPolling()
  const interval = scope === 'full' ? 3000 : 10000
  fetchWorkflows()
  if (scope === 'full') fetchDeadLetters()
  pollTimer = setInterval(() => {
    fetchWorkflows()
    if (scope === 'full') fetchDeadLetters()
  }, interval)
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  stopDetailPolling()
}

function startDetailPolling(id: string) {
  stopDetailPolling()
  fetchDetail(id)
  detailTimer = setInterval(() => fetchDetail(id), 3000)
}

function stopDetailPolling() {
  if (detailTimer) {
    clearInterval(detailTimer)
    detailTimer = null
  }
}

function selectWorkflow(id: string) {
  selectedWorkflowId = id
  workflowDetail = null
  phases = []
  tokens = null
  startDetailPolling(id)
}

function clearSelection() {
  selectedWorkflowId = null
  workflowDetail = null
  phases = []
  tokens = null
  verdicts = []
  personaOutputs = {}
  outputModalPersona = null
  stopDetailPolling()
}

async function pauseWorkflow(workflowID: string, reason: string): Promise<ActionResult | null> {
  try {
    // @ts-ignore
    const result = await window.go.main.App.PauseWorkflow(workflowID, reason)
    await fetchWorkflows()
    return result
  } catch (e) {
    error = e instanceof Error ? e.message : String(e)
    return null
  }
}

async function cancelWorkflow(workflowID: string, reason: string): Promise<ActionResult | null> {
  try {
    // @ts-ignore
    const result = await window.go.main.App.CancelWorkflow(workflowID, reason)
    await fetchWorkflows()
    return result
  } catch (e) {
    error = e instanceof Error ? e.message : String(e)
    return null
  }
}

async function resumeWorkflow(workflowID: string, reason: string): Promise<ActionResult | null> {
  try {
    // @ts-ignore
    const result = await window.go.main.App.ResumeWorkflow(workflowID, reason)
    await fetchWorkflows()
    return result
  } catch (e) {
    error = e instanceof Error ? e.message : String(e)
    return null
  }
}

async function approveHint(workflowID: string, persona: string, guidance: string): Promise<ActionResult | null> {
  try {
    // @ts-ignore
    const result = await window.go.main.App.ApproveHint(workflowID, persona, guidance)
    await fetchWorkflows()
    if (selectedWorkflowId === workflowID) await fetchDetail(workflowID)
    return result
  } catch (e) {
    error = e instanceof Error ? e.message : String(e)
    return null
  }
}

async function rejectHint(workflowID: string, persona: string, reason: string, action: string): Promise<ActionResult | null> {
  try {
    // @ts-ignore
    const result = await window.go.main.App.RejectHint(workflowID, persona, reason, action)
    await fetchWorkflows()
    if (selectedWorkflowId === workflowID) await fetchDetail(workflowID)
    return result
  } catch (e) {
    error = e instanceof Error ? e.message : String(e)
    return null
  }
}

async function injectGuidance(workflowID: string, content: string, target: string): Promise<ActionResult | null> {
  try {
    // @ts-ignore
    const result = await window.go.main.App.InjectGuidance(workflowID, content, target)
    await fetchWorkflows()
    return result
  } catch (e) {
    error = e instanceof Error ? e.message : String(e)
    return null
  }
}

export const dashboardStore = {
  get workflows() { return sortedWorkflows },
  get selectedWorkflowId() { return selectedWorkflowId },
  get workflowDetail() { return workflowDetail },
  get phases() { return phases },
  get tokens() { return tokens },
  get deadLetterCount() { return deadLetterCount },
  get serverReachable() { return serverReachable },
  get error() { return error },
  get activeCount() { return activeCount },
  get failedCount() { return failedCount },
  get pausedCount() { return pausedCount },
  get verdicts() { return verdicts },
  get personaOutputs() { return personaOutputs },
  get outputModalPersona() { return outputModalPersona },
  startPolling,
  stopPolling,
  selectWorkflow,
  clearSelection,
  pauseWorkflow,
  cancelWorkflow,
  resumeWorkflow,
  injectGuidance,
  approveHint,
  rejectHint,
  fetchWorkflows,
  fetchPersonaOutput,
  closeOutputModal,
}
