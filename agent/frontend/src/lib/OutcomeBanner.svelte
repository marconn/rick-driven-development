<script lang="ts">
  import type { WorkflowSummary, WorkflowDetail, PhaseEntry, TokenUsage, VerdictRecord } from '../stores/dashboard.svelte'
  import { dashboardStore } from '../stores/dashboard.svelte'

  let { workflow, detail, phases, tokens, verdicts }: {
    workflow: WorkflowSummary
    detail: WorkflowDetail
    phases: PhaseEntry[]
    tokens: TokenUsage | null
    verdicts: VerdictRecord[]
  } = $props()

  function formatDuration(ms: number): string {
    const s = Math.floor(ms / 1000)
    if (s < 60) return `${s}s`
    const m = Math.floor(s / 60)
    const rs = s % 60
    if (m < 60) return `${m}m ${rs}s`
    const h = Math.floor(m / 60)
    return `${h}h ${m % 60}m`
  }

  let duration = $derived.by(() => {
    if (!workflow.started_at) return ''
    const start = new Date(workflow.started_at)
    const end = workflow.completed_at ? new Date(workflow.completed_at) : new Date()
    return formatDuration(end.getTime() - start.getTime())
  })

  let statusLine = $derived.by(() => {
    const d = duration
    switch (workflow.status) {
      case 'completed': {
        const phaseCount = phases.filter(p => p.status === 'completed').length
        const totalTokens = tokens?.total ?? detail.tokens_used ?? 0
        const parts = [`Completed in ${d}`]
        if (phaseCount > 0) parts.push(`${phaseCount} phase${phaseCount !== 1 ? 's' : ''}`)
        if (totalTokens > 0) parts.push(`${totalTokens.toLocaleString()} tokens`)
        return parts.join(' · ')
      }
      case 'failed':
        return `Failed after ${d}${workflow.fail_reason ? ' · ' + workflow.fail_reason : ''}`
      case 'cancelled': {
        const completed = phases.filter(p => p.status === 'completed').length
        const total = phases.length
        return `Cancelled after ${d} · ${completed}/${total} phases completed`
      }
      default:
        return ''
    }
  })

  let groupedVerdicts = $derived.by(() => {
    if (!verdicts.length) return []

    const map = new Map<string, VerdictRecord[]>()
    for (const v of verdicts) {
      const key = v.source_phase
      if (!map.has(key)) map.set(key, [])
      map.get(key)!.push(v)
    }

    const entries = Array.from(map.entries()).map(([source_phase, records]) => {
      const hasFail = records.some(r => r.outcome === 'fail')
      const outcome = hasFail ? 'fail' : 'pass'
      const issues = records.flatMap(r => r.issues ?? [])
      const summary = records.find(r => r.outcome === outcome)?.summary ?? records[0].summary
      return { source_phase, outcome, summary, issues, records }
    })

    return entries.sort((a, b) => {
      if (a.outcome === b.outcome) return a.source_phase < b.source_phase ? -1 : 1
      return a.outcome === 'fail' ? -1 : 1
    })
  })

  let retries = $derived(
    Object.entries(detail.feedback_count ?? {})
      .filter(([, count]) => count > 0)
      .sort((a, b) => b[1] - a[1])
  )

  let finalPersona = $derived.by(() => {
    if (workflow.status !== 'completed') return null
    const completed = phases.filter(p => p.status === 'completed' && p.completed_at)
    if (!completed.length) return null
    completed.sort((a, b) => {
      const ta = new Date(a.completed_at!).getTime()
      const tb = new Date(b.completed_at!).getTime()
      return tb - ta
    })
    return completed[0].phase
  })

  function issueSummary(issues: VerdictRecord['issues']): string {
    if (!issues?.length) return ''
    const counts: Record<string, number> = {}
    for (const issue of issues) {
      counts[issue.severity] = (counts[issue.severity] ?? 0) + 1
    }
    const order = ['critical', 'major', 'minor']
    return order
      .filter(s => counts[s])
      .map(s => `${counts[s]} ${s}`)
      .join(', ')
  }

  function outcomeBadge(outcome: string): string {
    switch (outcome) {
      case 'pass': return 'bg-emerald-50 text-emerald-700'
      case 'fail': return 'bg-red-50 text-red-600'
      default: return 'bg-gray-100 text-gray-500'
    }
  }

  function handleViewOutput() {
    if (finalPersona) {
      dashboardStore.fetchPersonaOutput(detail.id, finalPersona)
    }
  }
</script>

<div class="rounded-lg border border-gray-200 bg-gray-50/50 px-3 py-2.5 space-y-2">
  <div class="flex items-center gap-2">
    {#if workflow.status === 'completed'}
      <svg class="w-4 h-4 text-emerald-500 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5">
        <polyline points="20 6 9 17 4 12"/>
      </svg>
    {:else if workflow.status === 'failed'}
      <svg class="w-4 h-4 text-red-400 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5">
        <line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/>
      </svg>
    {:else}
      <svg class="w-4 h-4 text-gray-400 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5">
        <line x1="5" y1="12" x2="19" y2="12"/>
      </svg>
    {/if}
    <span class="text-sm text-gray-600">{statusLine}</span>
  </div>

  {#if groupedVerdicts.length > 0}
    <div class="space-y-1 pl-6">
      {#each groupedVerdicts as entry}
        <div class="flex items-center gap-2 flex-wrap">
          <span class="px-1.5 py-0.5 text-xs rounded-full {outcomeBadge(entry.outcome)}">{entry.outcome}</span>
          <span class="text-sm text-gray-500">{entry.source_phase}</span>
          {#if entry.outcome === 'fail' && entry.issues.length > 0}
            <span class="text-xs text-gray-400">({issueSummary(entry.issues)})</span>
          {/if}
        </div>
      {/each}
    </div>
  {/if}

  {#if retries.length > 0}
    <div class="pl-6 flex flex-wrap gap-x-3 gap-y-0.5">
      {#each retries as [persona, count]}
        <span class="text-sm text-amber-600">{persona} retried {count}&times;</span>
      {/each}
    </div>
  {/if}

  {#if finalPersona}
    <div class="pl-6">
      <button
        onclick={handleViewOutput}
        class="text-sm text-blue-500 hover:text-blue-600 transition-colors underline underline-offset-2"
      >
        View {finalPersona} output
      </button>
    </div>
  {/if}
</div>
