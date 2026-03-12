<script lang="ts">
  import type { VerdictRecord } from '../stores/dashboard.svelte'

  let { verdicts }: { verdicts: VerdictRecord[] } = $props()

  let expanded = $state<Set<number>>(new Set())

  function toggleExpand(index: number) {
    const next = new Set(expanded)
    if (next.has(index)) {
      next.delete(index)
    } else {
      next.add(index)
    }
    expanded = next
  }

  let sorted = $derived(
    [...verdicts].sort((a, b) => {
      const order: Record<string, number> = { fail: 0, unknown: 1, pass: 2 }
      return (order[a.outcome] ?? 1) - (order[b.outcome] ?? 1)
    })
  )

  function outcomeBadge(outcome: string): string {
    switch (outcome) {
      case 'pass': return 'bg-emerald-50 text-emerald-700'
      case 'fail': return 'bg-red-50 text-red-600'
      default: return 'bg-gray-100 text-gray-500'
    }
  }

  function severityBadge(severity: string): string {
    switch (severity) {
      case 'critical': return 'text-red-600'
      case 'major': return 'text-amber-600'
      default: return 'text-gray-500'
    }
  }
</script>

<div>
  <h4 class="text-base font-medium text-gray-500 mb-2">Review Verdicts</h4>
  <div class="space-y-2">
    {#each sorted as verdict, i}
      <div class="border border-gray-200 rounded-lg bg-white">
        <button
          onclick={() => toggleExpand(i)}
          class="w-full px-3 py-2 flex items-center justify-between text-left hover:bg-gray-50 transition-colors rounded-lg"
        >
          <div class="flex items-center gap-2">
            <span class="px-2 py-0.5 text-sm rounded-full {outcomeBadge(verdict.outcome)}">{verdict.outcome}</span>
            <span class="text-sm text-gray-400">{verdict.source_phase}</span>
            <span class="text-sm text-gray-300">&rarr;</span>
            <span class="text-sm text-gray-600">{verdict.phase}</span>
          </div>
          <div class="flex items-center gap-2">
            {#if verdict.issues && verdict.issues.length > 0}
              <span class="text-sm text-gray-400">{verdict.issues.length} issue{verdict.issues.length !== 1 ? 's' : ''}</span>
            {/if}
            <svg class="w-4 h-4 text-gray-400 transition-transform {expanded.has(i) ? 'rotate-180' : ''}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <polyline points="6 9 12 15 18 9"/>
            </svg>
          </div>
        </button>

        {#if expanded.has(i)}
          <div class="px-3 pb-3 space-y-2">
            <p class="text-sm text-gray-600 leading-relaxed">{verdict.summary}</p>
            {#if verdict.issues && verdict.issues.length > 0}
              <div class="space-y-1">
                {#each verdict.issues as issue}
                  <div class="flex items-start gap-2 pl-2 border-l-2 border-gray-200">
                    <span class="text-xs font-medium {severityBadge(issue.severity)} shrink-0">{issue.severity}</span>
                    <span class="text-xs text-gray-400 shrink-0">{issue.category}</span>
                    <span class="text-sm text-gray-600 flex-1">{issue.description}</span>
                    {#if issue.file}
                      <span class="text-xs text-gray-400 font-mono shrink-0">{issue.file}{issue.line ? `:${issue.line}` : ''}</span>
                    {/if}
                  </div>
                {/each}
              </div>
            {/if}
          </div>
        {/if}
      </div>
    {/each}
  </div>
</div>
