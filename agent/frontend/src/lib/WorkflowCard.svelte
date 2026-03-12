<script lang="ts">
  import type { WorkflowSummary } from '../stores/dashboard.svelte'
  import { dashboardStore } from '../stores/dashboard.svelte'
  import PhaseTimeline from './PhaseTimeline.svelte'
  import HintReviewPanel from './HintReviewPanel.svelte'
  import WorkflowActions from './WorkflowActions.svelte'
  import WorkflowDetailView from './WorkflowDetail.svelte'
  import PersonaOutputPanel from './PersonaOutputPanel.svelte'
  import OutcomeBanner from './OutcomeBanner.svelte'

  let { workflow }: { workflow: WorkflowSummary } = $props()

  let isSelected = $derived(dashboardStore.selectedWorkflowId === workflow.aggregate_id)
  let detail = $derived(dashboardStore.workflowDetail)
  let phases = $derived(dashboardStore.phases)
  let tokens = $derived(dashboardStore.tokens)

  let shortId = $derived(workflow.aggregate_id.substring(0, 8))
  let pendingHints = $derived(isSelected && detail?.pending_hints?.length ? detail.pending_hints : [])
  let verdicts = $derived(dashboardStore.verdicts)

  let verdictSummary = $derived.by(() => {
    if (!isSelected || !verdicts.length) return ''
    const pass = verdicts.filter(v => v.outcome === 'pass').length
    const fail = verdicts.filter(v => v.outcome === 'fail').length
    const parts: string[] = []
    if (pass > 0) parts.push(`${pass} pass`)
    if (fail > 0) parts.push(`${fail} fail`)
    return parts.join(', ')
  })

  let elapsed = $derived.by(() => {
    if (!workflow.started_at) return ''
    const start = new Date(workflow.started_at)
    if (workflow.completed_at) {
      const end = new Date(workflow.completed_at)
      const ms = end.getTime() - start.getTime()
      return formatDuration(ms)
    }
    const ms = Date.now() - start.getTime()
    return formatDuration(ms)
  })

  function formatDuration(ms: number): string {
    const s = Math.floor(ms / 1000)
    if (s < 60) return `${s}s`
    const m = Math.floor(s / 60)
    const rs = s % 60
    if (m < 60) return `${m}m ${rs}s`
    const h = Math.floor(m / 60)
    return `${h}h ${m % 60}m`
  }

  let statusDot = $derived.by(() => {
    switch (workflow.status) {
      case 'running': return 'bg-blue-400 status-dot-running'
      case 'completed': return 'bg-emerald-500'
      case 'failed': return 'bg-red-400'
      case 'paused': return 'bg-amber-400'
      case 'cancelled': return 'bg-gray-400'
      default: return 'bg-gray-400'
    }
  })

  let statusBadge = $derived.by(() => {
    switch (workflow.status) {
      case 'running': return 'text-blue-500'
      case 'completed': return 'text-emerald-600'
      case 'failed': return 'text-red-500'
      case 'paused': return 'text-amber-500'
      case 'cancelled': return 'text-gray-400'
      default: return 'text-gray-400'
    }
  })

  let isTerminal = $derived(
    workflow.status === 'completed' || workflow.status === 'failed' || workflow.status === 'cancelled'
  )

  function toggleDetail() {
    if (isSelected) {
      dashboardStore.clearSelection()
    } else {
      dashboardStore.selectWorkflow(workflow.aggregate_id)
    }
  }
</script>

<div class="rounded-lg border border-gray-200 bg-white overflow-hidden">
  <!-- Header -->
  <div class="flex items-center justify-between px-4 py-3">
    <div class="flex items-center gap-3">
      <div class="w-2.5 h-2.5 rounded-full {statusDot}"></div>
      <span class="text-base font-mono text-gray-400">{shortId}</span>
      <span class="text-base text-gray-500">{workflow.workflow_id}</span>
      <span class="text-base font-medium {statusBadge}">{workflow.status}</span>
      {#if pendingHints.length > 0}
        <span class="text-base text-teal-600 animate-pulse">
          {pendingHints.length} hint{pendingHints.length > 1 ? 's' : ''}
        </span>
      {/if}
      {#if verdictSummary}
        <span class="text-base {verdicts.some(v => v.outcome === 'fail') ? 'text-red-500' : 'text-emerald-600'}">
          {verdictSummary}
        </span>
      {/if}
    </div>

    <div class="flex items-center gap-3">
      <span class="text-base text-gray-400">{elapsed}</span>
      <WorkflowActions {workflow} />
    </div>
  </div>

  {#if pendingHints.length > 0}
    <div class="px-4 pb-2">
      <HintReviewPanel hints={pendingHints} workflowId={workflow.aggregate_id} />
    </div>
  {/if}

  {#if isSelected && detail && isTerminal}
    <div class="px-4 pb-2">
      <OutcomeBanner {workflow} {detail} {phases} tokens={tokens} verdicts={verdicts} />
    </div>
  {/if}

  {#if isSelected && phases.length > 0}
    <div class="px-4 pb-2">
      <PhaseTimeline {phases} hintedPersonas={pendingHints.map(h => h.persona)} />
    </div>
  {/if}

  <div class="border-t border-gray-100 px-4 py-2 flex items-center justify-between">
    <button
      onclick={toggleDetail}
      class="text-base text-gray-400 hover:text-gray-600 transition-colors"
    >
      {isSelected ? 'Hide details' : 'Show details'}
    </button>

    {#if isSelected && tokens}
      <span class="text-base text-gray-400">
        Tokens: {tokens.total.toLocaleString()}
      </span>
    {:else if !isSelected && isTerminal}
      <span class="text-base truncate max-w-sm {workflow.status === 'failed' ? 'text-red-500' : 'text-gray-400'}">
        {#if workflow.status === 'completed'}
          Completed{elapsed ? ` in ${elapsed}` : ''}
        {:else if workflow.status === 'failed'}
          Failed{elapsed ? ` after ${elapsed}` : ''}{workflow.fail_reason ? ` · ${workflow.fail_reason}` : ''}
        {:else}
          Cancelled{elapsed ? ` after ${elapsed}` : ''}
        {/if}
      </span>
    {/if}
  </div>

  {#if isSelected && detail}
    <WorkflowDetailView {detail} {phases} {tokens} {verdicts} />
  {/if}

  <PersonaOutputPanel />
</div>
